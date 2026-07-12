package anthropic

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GreyGunG/grokbuild-proxy/internal/config"
	"github.com/GreyGunG/grokbuild-proxy/internal/lb"
)

// PostResponsesFunc posts a Responses body to upstream and returns the raw HTTP response.
// body is already translated JSON. stream indicates Accept preference.
// Caller of HandleMessages closes resp.Body.
type PostResponsesFunc func(ctx context.Context, model, convID string, body []byte, stream bool) (*http.Response, error)

// Handlers serves Anthropic Messages endpoints.
type Handlers struct {
	Post PostResponsesFunc
	Cfg  config.AnthropicConfig
	// ResolveModel maps client model → upstream. If nil, uses Cfg.ModelAliases + passthrough.
	ResolveModel func(string) string
	MaxBody      int64
}

func (h *Handlers) maxBody() int64 {
	if h != nil && h.MaxBody > 0 {
		return h.MaxBody
	}
	return 20 << 20
}

// resolve applies ResolveModel or config aliases.
func (h *Handlers) resolve(model string) string {
	if h.ResolveModel != nil {
		return h.ResolveModel(model)
	}
	return h.Cfg.ResolveModel(model)
}

// HandleMessages serves POST /v1/messages (query ?beta=true is ignored).
func (h *Handlers) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.Post == nil {
		WriteError(w, http.StatusInternalServerError, "upstream not configured")
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody()))
	if err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		WriteError(w, status, "failed to read body")
		return
	}
	_ = r.Body.Close()

	// Auto-compact + tool_result truncation + hard context guard.
	guardCfg := ContextGuardConfig{
		MaxInputTokens:     h.Cfg.EffectiveMaxInputTokens(),
		SoftInputTokens:    h.Cfg.EffectiveSoftInputTokens(),
		MaxToolResultChars: h.Cfg.EffectiveMaxToolResultChars(),
		KeepRecentMessages: h.Cfg.EffectiveKeepRecentMessages(),
		PreserveCacheHints: h.Cfg.PreserveCacheHintsEnabled(),
		AutoCompact:        h.Cfg.AutoCompactEnabled(),
	}
	prepared, compact, gerr := PrepareAnthropicBody(raw, guardCfg)
	if gerr != nil {
		WriteError(w, http.StatusBadRequest, gerr.Error())
		return
	}
	if compact.Applied {
		w.Header().Set("X-Grokbuild-Context-Compact", "1")
		if compact.DroppedMessages > 0 {
			w.Header().Set("X-Grokbuild-Dropped-Messages", fmt.Sprintf("%d", compact.DroppedMessages))
		}
		if compact.TruncatedToolResults > 0 {
			w.Header().Set("X-Grokbuild-Truncated-Tool-Results", fmt.Sprintf("%d", compact.TruncatedToolResults))
		}
		w.Header().Set("X-Grokbuild-Input-Tokens-Before", fmt.Sprintf("%d", compact.EstimatedBefore))
		w.Header().Set("X-Grokbuild-Input-Tokens-After", fmt.Sprintf("%d", compact.EstimatedAfter))
	}
	raw = prepared

	var probe struct {
		Model    string          `json:"model"`
		Stream   bool            `json:"stream"`
		Thinking json.RawMessage `json:"thinking"`
	}
	_ = json.Unmarshal(raw, &probe)

	resolved := h.resolve(probe.Model)
	convID := sessionIDFromRequest(r, raw)
	thinkingBridge := thinkingBridgeFromRaw(probe.Thinking)

	body, originalModel, stream, err := TranslateRequest(raw, TranslateReqOptions{
		ResolvedModel:      resolved,
		ConvID:             convID,
		StripUnknownBetas:  h.Cfg.StripUnknownBetas,
		PreserveCacheHints: h.Cfg.PreserveCacheHintsEnabled(),
		MaxToolResultChars: h.Cfg.EffectiveMaxToolResultChars(),
	})
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := h.Post(r.Context(), resolved, convID, body, stream)
	if err != nil {
		if errors.Is(err, lb.ErrNoCredential) {
			w.Header().Set("Retry-After", "1")
			WriteError(w, http.StatusServiceUnavailable, "no usable upstream credentials")
			return
		}
		WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer resp.Body.Close()
	copyAnthropicUpstreamHeaders(w.Header(), resp.Header)

	if resp.StatusCode >= 400 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		msg := FormatErrorMessage(resp.StatusCode, rawErr)
		if IsContextTooLongMessage(msg) || IsContextTooLongMessage(string(rawErr)) {
			WriteError(w, http.StatusBadRequest, "context_too_long: "+msg+"; try a new session or smaller tool outputs")
			return
		}
		WriteError(w, resp.StatusCode, msg)
		return
	}

	ct := resp.Header.Get("Content-Type")
	isSSE := stream || strings.Contains(ct, "text/event-stream")

	if isSSE {
		WriteClaudeSSEHeaders(w)
		var flusher http.Flusher
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
		reqModel := probe.Model
		if reqModel == "" {
			reqModel = originalModel
		}
		tr := NewStreamTranslator(w, flusher, reqModel, thinkingBridge)
		if err := PipeResponsesSSE(resp.Body, tr); err != nil {
			if tr.State.Started && !tr.State.Finished {
				_ = tr.Fail(http.StatusBadGateway, err.Error())
				return
			}
			if tr.State.Finished {
				return
			}
			WriteError(w, http.StatusBadGateway, err.Error())
			return
		}
		return
	}

	// Non-stream JSON
	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		WriteError(w, http.StatusBadGateway, "failed to read upstream body")
		return
	}
	msg, err := TranslateResponse(rawResp, TranslateRespOptions{
		RequestModel:  probe.Model,
		FallbackModel: resolved,
		Thinking:      thinkingBridge,
	})
	if err != nil {
		WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(msg)
}

func copyAnthropicUpstreamHeaders(dst, src http.Header) {
	for _, key := range []string{
		"Retry-After",
		"Request-Id",
		"Anthropic-Request-Id",
	} {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
	if value := src.Get("X-Request-Id"); value != "" {
		dst.Set("X-Upstream-Request-Id", value)
	}
}

// HandleCountTokens returns a conservative local estimate. Grok Build does not
// expose Anthropic's tokenizer, so this is intentionally an estimate, not zero.
func (h *Handlers) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !h.Cfg.CountTokens {
		WriteError(w, http.StatusNotFound, "count_tokens is disabled")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody()))
	if err != nil {
		WriteError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	tokens := EstimateRawTokens(raw)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"input_tokens": tokens,
		"estimate":     true,
		"note":         "heuristic estimate (Grok Build has no Anthropic tokenizer)",
	})
}

// sessionIDFromRequest extracts sticky conv id for Grok prompt cache.
func sessionIDFromRequest(r *http.Request, raw ...[]byte) string {
	if r == nil {
		return newSessionID()
	}
	for _, k := range []string{
		"x-claude-code-session-id",
		"X-Claude-Code-Session-Id",
		"x-session-id",
		"x-grok-conv-id",
	} {
		if v := strings.TrimSpace(r.Header.Get(k)); v != "" {
			return v
		}
	}
	if len(raw) > 0 {
		var probe struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		if json.Unmarshal(raw[0], &probe) == nil && strings.TrimSpace(probe.Metadata.UserID) != "" {
			sum := sha256.Sum256([]byte(strings.TrimSpace(probe.Metadata.UserID)))
			return "sess_meta_" + hex.EncodeToString(sum[:12])
		}
	}
	return newSessionID()
}

func countStringBytes(value any) int {
	switch v := value.(type) {
	case string:
		return len(v)
	case []any:
		total := 0
		for _, item := range v {
			total += countStringBytes(item)
		}
		return total
	case map[string]any:
		total := 0
		for key, item := range v {
			if key != "model" && key != "type" {
				total += len(key)
			}
			total += countStringBytes(item)
		}
		return total
	default:
		return 0
	}
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "sess_fallback"
	}
	return "sess_" + hex.EncodeToString(b[:])
}

// Register attaches Anthropic routes on mux (optional helper).
func (h *Handlers) Register(mux *http.ServeMux) {
	if mux == nil {
		return
	}
	mux.HandleFunc("/v1/messages", h.HandleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", h.HandleCountTokens)
}
