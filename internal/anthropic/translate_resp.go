package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MessageResponse is the Anthropic non-stream message response.
type MessageResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	StopSeq    *string        `json:"stop_sequence"`
	Usage      Usage          `json:"usage"`
}

// ContentBlock is a Claude content block.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  *string         `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// Usage is Claude usage.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// TranslateRespOptions controls Responses → Anthropic conversion.
type TranslateRespOptions struct {
	// RequestModel is the original client model alias (preferred in response.model).
	RequestModel string
	// FallbackModel used when response has no model and RequestModel empty.
	FallbackModel string
	// Thinking enables CPA-style Responses reasoning → Anthropic thinking blocks.
	Thinking ThinkingBridgeOptions
}

// TranslateResponse converts a Grok/OpenAI Responses JSON body into an Anthropic message.
// Accepts either a full response object, or a stream terminal envelope
// {"type":"response.completed","response":{...}}.
func TranslateResponse(raw []byte, opts TranslateRespOptions) (*MessageResponse, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil, fmt.Errorf("anthropic: empty responses body")
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("anthropic: invalid responses body: %w", err)
	}

	// Unwrap response.completed / response.incomplete envelopes.
	if t := rawString(root["type"]); t == "response.completed" || t == "response.incomplete" {
		if resp, ok := root["response"]; ok && len(resp) > 0 {
			raw = resp
			root = nil
			if err := json.Unmarshal(raw, &root); err != nil {
				return nil, fmt.Errorf("anthropic: invalid nested response: %w", err)
			}
		}
	}

	id := normalizeMessageID(rawString(root["id"]))

	model := opts.RequestModel
	if model == "" {
		model = rawString(root["model"])
	}
	if model == "" {
		model = opts.FallbackModel
	}

	msg := &MessageResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      model,
		Content:    nil,
		StopReason: "end_turn",
		StopSeq:    nil,
		Usage:      Usage{},
	}

	if uRaw, ok := root["usage"]; ok && len(uRaw) > 0 {
		msg.Usage = extractUsage(uRaw)
	}

	hasTool := false
	if outRaw, ok := root["output"]; ok && len(outRaw) > 0 {
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(outRaw, &items); err == nil {
			for _, item := range items {
				typ := rawString(item["type"])
				switch typ {
				case "message":
					if cRaw, ok := item["content"]; ok {
						msg.Content = append(msg.Content, extractMessageTextBlocks(cRaw)...)
					}
				case "function_call":
					hasTool = true
					callID := normalizeToolUseID(firstNonEmpty(rawString(item["call_id"]), rawString(item["id"])))
					name := rawString(item["name"])
					args := "{}"
					if a, ok := item["arguments"]; ok && len(a) > 0 {
						var s string
						if err := json.Unmarshal(a, &s); err == nil {
							if s == "" {
								s = "{}"
							}
							args = s
						} else if json.Valid(a) {
							args = string(a)
						}
					}
					if !json.Valid([]byte(args)) {
						args = "{}"
					}
					if !strings.HasPrefix(strings.TrimSpace(args), "{") {
						args = "{}"
					}
					msg.Content = append(msg.Content, ContentBlock{
						Type:  "tool_use",
						ID:    callID,
						Name:  name,
						Input: json.RawMessage(args),
					})
				case "reasoning":
					if !opts.Thinking.Enabled {
						continue
					}
					summary := extractReasoningSummary(item["summary"])
					if opts.Thinking.Display == "omitted" {
						summary = ""
					}
					signature := rawString(item["encrypted_content"])
					if summary == "" && signature == "" {
						continue
					}
					msg.Content = append(msg.Content, ContentBlock{
						Type:      "thinking",
						Thinking:  stringPointer(summary),
						Signature: signature,
					})
				default:
					continue
				}
			}
		}
	}

	if len(msg.Content) == 0 {
		if t := rawString(root["output_text"]); t != "" {
			msg.Content = []ContentBlock{{Type: "text", Text: t}}
		}
	}
	if msg.Content == nil {
		msg.Content = []ContentBlock{}
	}

	msg.StopReason = mapStopReason(root, hasTool)
	return msg, nil
}

func extractReasoningSummary(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var summary strings.Builder
	for _, part := range parts {
		text := rawString(part["text"])
		if text == "" {
			text = rawString(part["content"])
		}
		if text == "" {
			continue
		}
		if summary.Len() > 0 {
			summary.WriteByte('\n')
		}
		summary.WriteString(text)
	}
	return summary.String()
}

func stringPointer(value string) *string {
	return &value
}

func extractMessageTextBlocks(cRaw json.RawMessage) []ContentBlock {
	var out []ContentBlock
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(cRaw, &parts); err == nil {
		for _, p := range parts {
			pt := rawString(p["type"])
			switch pt {
			case "output_text", "text", "input_text":
				text := rawString(p["text"])
				if text != "" {
					out = append(out, ContentBlock{Type: "text", Text: text})
				}
			}
		}
		return out
	}
	var s string
	if err := json.Unmarshal(cRaw, &s); err == nil && s != "" {
		out = append(out, ContentBlock{Type: "text", Text: s})
	}
	return out
}

func extractUsage(uRaw json.RawMessage) Usage {
	var u struct {
		InputTokens      int64 `json:"input_tokens"`
		OutputTokens     int64 `json:"output_tokens"`
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
		InputTokenCount  int64 `json:"input_token_count"`
		OutputTokenCount int64 `json:"output_token_count"`
	}
	_ = json.Unmarshal(uRaw, &u)
	in := u.InputTokens
	if in == 0 {
		in = u.PromptTokens
	}
	if in == 0 {
		in = u.InputTokenCount
	}
	out := u.OutputTokens
	if out == 0 {
		out = u.CompletionTokens
	}
	if out == 0 {
		out = u.OutputTokenCount
	}
	return Usage{InputTokens: in, OutputTokens: out}
}

func mapStopReason(root map[string]json.RawMessage, hasTool bool) string {
	if hasTool {
		return "tool_use"
	}
	if sr := rawString(root["stop_reason"]); sr != "" {
		return normalizeStopReason(sr, false)
	}
	if idRaw, ok := root["incomplete_details"]; ok {
		var d struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(idRaw, &d) == nil && d.Reason != "" {
			return normalizeStopReason(d.Reason, false)
		}
	}
	if st := rawString(root["status"]); st == "incomplete" {
		return "max_tokens"
	}
	return "end_turn"
}

func normalizeStopReason(s string, hasTool bool) string {
	if hasTool {
		return "tool_use"
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "stop", "completed", "end_turn":
		return "end_turn"
	case "max_tokens", "max_output_tokens", "length":
		return "max_tokens"
	case "tool_use", "tool_calls", "function_call":
		return "end_turn"
	case "stop_sequence", "pause_turn", "refusal":
		return s
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
