package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/GreyGunG/grokbuild-proxy/internal/anthropic"
	"github.com/GreyGunG/grokbuild-proxy/internal/lb"
)

// HandleResponses serves POST /v1/responses.
// It sanitizes the body, calls the injected Post func, and proxies stream/non-stream.
func (h *Handlers) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Post == nil {
		WriteError(w, http.StatusInternalServerError, "responses handler not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	raw, err := h.readBody(r)
	if err != nil {
		if err == errBodyTooLarge {
			WriteError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "body_too_large")
			return
		}
		WriteError(w, http.StatusBadRequest, "failed to read body: "+err.Error(), "invalid_request_error", "invalid_body")
		return
	}

	raw, err = h.applyContextGuard(w, raw)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "context_too_long")
		return
	}

	convHint := convIDFromRequest(r, extractBodyConvID(raw))
	res, err := SanitizeResponses(raw, convHint)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_body")
		return
	}
	sanitized, err := marshalBody(res.Body)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to encode request", "server_error", "encode_error")
		return
	}
	sanitized, err = h.applyContextGuard(w, sanitized)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "context_too_long")
		return
	}

	resp, err := h.Post(r.Context(), res.Model, res.ConvID, sanitized, res.Stream)
	if err != nil {
		writePostError(w, err)
		return
	}
	if resp == nil {
		WriteError(w, http.StatusBadGateway, "upstream returned nil response", "server_error", "upstream_error")
		return
	}

	if res.Stream || isSSEContentType(resp.Header.Get("Content-Type")) {
		if resp.StatusCode >= 400 {
			rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if anthropic.IsContextTooLongMessage(string(rawErr)) {
				WriteError(w, http.StatusBadRequest, "context_too_long: "+trimErr(rawErr)+"; try a new session or smaller tool outputs", "invalid_request_error", "context_too_long")
				return
			}
			MapUpstreamError(w, resp.StatusCode, rawErr)
			return
		}
		streamUpstreamSSE(w, resp)
		return
	}
	if resp.StatusCode >= 400 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if anthropic.IsContextTooLongMessage(string(rawErr)) {
			WriteError(w, http.StatusBadRequest, "context_too_long: "+trimErr(rawErr)+"; try a new session or smaller tool outputs", "invalid_request_error", "context_too_long")
			return
		}
		MapUpstreamError(w, resp.StatusCode, rawErr)
		return
	}
	proxyUpstreamJSON(w, resp)
}

// HandleChatCompletions serves POST /v1/chat/completions.
// Converts chat → Responses, posts upstream, converts non-stream Responses → chat.completion.
// Stream chat is best-effort: converts output_text.delta events when possible.
func (h *Handlers) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Post == nil {
		WriteError(w, http.StatusInternalServerError, "chat handler not configured", "server_error", "not_configured")
		return
	}
	if r.Method != http.MethodPost {
		WriteError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
		return
	}

	raw, err := h.readBody(r)
	if err != nil {
		if err == errBodyTooLarge {
			WriteError(w, http.StatusRequestEntityTooLarge, "request body too large", "invalid_request_error", "body_too_large")
			return
		}
		WriteError(w, http.StatusBadRequest, "failed to read body: "+err.Error(), "invalid_request_error", "invalid_body")
		return
	}

	// Compact chat history before conversion so huge tool dumps never reach upstream.
	raw, err = h.applyContextGuard(w, raw)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "context_too_long")
		return
	}

	body, err := ChatToResponses(raw)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_body")
		return
	}

	convHint := convIDFromRequest(r, extractBodyConvID(raw))
	sanitizedRes, err := SanitizeResponses(body, convHint)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_body")
		return
	}
	sanitized, err := marshalBody(sanitizedRes.Body)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to encode request", "server_error", "encode_error")
		return
	}
	sanitized, err = h.applyContextGuard(w, sanitized)
	if err != nil {
		WriteError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "context_too_long")
		return
	}

	stream := sanitizedRes.Stream
	model := sanitizedRes.Model
	convID := sanitizedRes.ConvID
	if convID == "" {
		convID = convHint
	}

	resp, err := h.Post(r.Context(), model, convID, sanitized, stream)
	if err != nil {
		writePostError(w, err)
		return
	}
	if resp == nil {
		WriteError(w, http.StatusBadGateway, "upstream returned nil response", "server_error", "upstream_error")
		return
	}
	copyUpstreamResponseHeaders(w.Header(), resp.Header)

	if stream {
		handleChatStream(w, resp, chatStreamIncludesUsage(raw))
		return
	}

	defer resp.Body.Close()
	upRaw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		WriteError(w, http.StatusBadGateway, "failed to read upstream response", "server_error", "upstream_read_error")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if anthropic.IsContextTooLongMessage(string(upRaw)) {
			WriteError(w, http.StatusBadRequest, "context_too_long: "+trimErr(upRaw)+"; try a new session or smaller tool outputs", "invalid_request_error", "context_too_long")
			return
		}
		MapUpstreamError(w, resp.StatusCode, upRaw)
		return
	}
	chatRaw, err := ResponsesToChat(upRaw)
	if err != nil {
		writeJSON(w, resp.StatusCode, upRaw)
		return
	}
	writeJSON(w, http.StatusOK, chatRaw)
}

func writePostError(w http.ResponseWriter, err error) {
	if errors.Is(err, lb.ErrNoCredential) {
		w.Header().Set("Retry-After", "1")
		WriteError(
			w,
			http.StatusServiceUnavailable,
			"no usable upstream credentials",
			"server_error",
			"credential_pool_unavailable",
		)
		return
	}
	WriteError(w, http.StatusBadGateway, "upstream request failed: "+err.Error(), "server_error", "upstream_error")
}

func (h *Handlers) applyContextGuard(w http.ResponseWriter, raw []byte) ([]byte, error) {
	if h == nil || h.ContextGuard == nil || len(raw) == 0 {
		return raw, nil
	}
	prepared, compact, err := anthropic.PrepareOpenAIBody(raw, *h.ContextGuard)
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
	if err != nil {
		return prepared, err
	}
	return prepared, nil
}

func trimErr(raw []byte) string {
	msg := strings.TrimSpace(string(raw))
	if msg == "" {
		return "upstream context overflow"
	}
	if len(msg) > 800 {
		return msg[:800]
	}
	return msg
}

func handleChatStream(w http.ResponseWriter, resp *http.Response, includeUsage bool) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if anthropic.IsContextTooLongMessage(string(raw)) {
			WriteError(w, http.StatusBadRequest, "context_too_long: "+trimErr(raw)+"; try a new session or smaller tool outputs", "invalid_request_error", "context_too_long")
			return
		}
		MapUpstreamError(w, resp.StatusCode, raw)
		return
	}

	if !isSSEContentType(resp.Header.Get("Content-Type")) {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			WriteError(w, http.StatusBadGateway, "failed to read upstream response", "server_error", "upstream_read_error")
			return
		}
		chatRaw, err := ResponsesToChat(raw)
		if err != nil {
			WriteError(w, http.StatusNotImplemented, "chat stream conversion not available for this upstream response", "server_error", "stream_not_supported")
			return
		}
		writeChatSSEFromCompletion(w, chatRaw)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	translator := newChatStreamTranslator(includeUsage)
	var processErr error
	scanErr := scanSSEDataLines(resp.Body, func(data []byte) bool {
		chunks, err := translator.process(data)
		if err != nil {
			processErr = err
			return false
		}
		for _, chunk := range chunks {
			raw, err := json.Marshal(chunk)
			if err != nil {
				processErr = err
				return false
			}
			if _, err := w.Write(append(append([]byte("data: "), raw...), '\n', '\n')); err != nil {
				processErr = err
				return false
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return true
	})

	if !translator.terminal {
		message := "upstream stream ended before a terminal event"
		if processErr != nil {
			message = processErr.Error()
		} else if scanErr != nil {
			message = scanErr.Error()
		}
		translator.terminal = true
		translator.failed = true
		raw, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": message,
			"type":    "server_error",
			"code":    "upstream_stream_truncated",
		}})
		_, _ = w.Write(append(append([]byte("data: "), raw...), '\n', '\n'))
	}

	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func chatStreamIncludesUsage(raw []byte) bool {
	var probe struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.StreamOptions.IncludeUsage
}

func writeChatSSEFromCompletion(w http.ResponseWriter, chatRaw []byte) {
	var comp map[string]any
	if err := json.Unmarshal(chatRaw, &comp); err != nil {
		WriteError(w, http.StatusBadGateway, "invalid chat conversion", "server_error", "convert_error")
		return
	}
	chunk := map[string]any{
		"id":      comp["id"],
		"object":  "chat.completion.chunk",
		"created": comp["created"],
		"model":   comp["model"],
		"choices": []any{},
	}
	if choices, ok := comp["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			delta := map[string]any{"role": "assistant"}
			if msg, ok := c0["message"].(map[string]any); ok {
				if content, exists := msg["content"]; exists {
					delta["content"] = content
				}
				if tcs, exists := msg["tool_calls"]; exists {
					delta["tool_calls"] = tcs
				}
			}
			chunk["choices"] = []any{
				map[string]any{
					"index":         0,
					"delta":         delta,
					"finish_reason": c0["finish_reason"],
				},
			}
		}
	}
	raw, err := marshalBody(chunk)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "encode error", "server_error", "encode_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(raw)
	_, _ = w.Write([]byte("\n\ndata: [DONE]\n\n"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// responsesSSEToChatChunk converts one Responses SSE JSON payload to a chat chunk.
func responsesSSEToChatChunk(data []byte, id, model *string) (chunk []byte, ok bool) {
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, false
	}
	typ := strings.ToLower(asString(ev["type"]))
	if typ == "" {
		if _, has := ev["output"]; has {
			chat := responsesMapToChat(ev)
			return completionToFinalChunk(chat), true
		}
		return nil, false
	}

	if resp, okm := ev["response"].(map[string]any); okm {
		if s := asString(resp["id"]); s != "" {
			*id = s
		}
		if s := asString(resp["model"]); s != "" {
			*model = s
		}
	}

	switch typ {
	case "response.output_text.delta", "response.text.delta":
		deltaText := asString(ev["delta"])
		if deltaText == "" {
			deltaText = asString(ev["text"])
		}
		if deltaText == "" {
			return nil, true
		}
		ch := map[string]any{
			"id":     *id,
			"object": "chat.completion.chunk",
			"model":  *model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"content": deltaText,
					},
					"finish_reason": nil,
				},
			},
		}
		b, err := marshalBody(ch)
		if err != nil {
			return nil, false
		}
		return b, true
	case "response.completed", "response.done":
		ch := map[string]any{
			"id":     *id,
			"object": "chat.completion.chunk",
			"model":  *model,
			"choices": []any{
				map[string]any{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": "stop",
				},
			},
		}
		b, err := marshalBody(ch)
		if err != nil {
			return nil, false
		}
		return b, true
	case "response.output_item.done":
		item, _ := ev["item"].(map[string]any)
		if item == nil {
			return nil, true
		}
		if strings.ToLower(asString(item["type"])) != "function_call" {
			return nil, true
		}
		name := asString(item["name"])
		args := asString(item["arguments"])
		callID := asString(item["call_id"])
		if callID == "" {
			callID = asString(item["id"])
		}
		ch := map[string]any{
			"id":     *id,
			"object": "chat.completion.chunk",
			"model":  *model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []any{
							map[string]any{
								"index": 0,
								"id":    callID,
								"type":  "function",
								"function": map[string]any{
									"name":      name,
									"arguments": args,
								},
							},
						},
					},
					"finish_reason": nil,
				},
			},
		}
		b, err := marshalBody(ch)
		if err != nil {
			return nil, false
		}
		return b, true
	default:
		return nil, true
	}
}

func completionToFinalChunk(chat map[string]any) []byte {
	delta := map[string]any{"role": "assistant"}
	finish := any("stop")
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if c0, ok := choices[0].(map[string]any); ok {
			if msg, ok := c0["message"].(map[string]any); ok {
				if c, exists := msg["content"]; exists {
					delta["content"] = c
				}
				if t, exists := msg["tool_calls"]; exists {
					delta["tool_calls"] = t
				}
			}
			if fr, exists := c0["finish_reason"]; exists {
				finish = fr
			}
		}
	}
	ch := map[string]any{
		"id":      chat["id"],
		"object":  "chat.completion.chunk",
		"created": chat["created"],
		"model":   chat["model"],
		"choices": []any{
			map[string]any{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			},
		},
	}
	b, _ := marshalBody(ch)
	return b
}
