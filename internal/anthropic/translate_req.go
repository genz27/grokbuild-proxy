package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TranslateReqOptions controls Claude → Responses conversion.
type TranslateReqOptions struct {
	// ResolvedModel is the upstream Grok model id after alias resolution.
	ResolvedModel string
	// ConvID becomes prompt_cache_key when set.
	ConvID string
	// StripUnknownBetas drops unrecognized fields inside supported beta configs.
	StripUnknownBetas bool
	// PreserveCacheHints annotates cache_control blocks for continuity.
	PreserveCacheHints bool
	// MaxToolResultChars truncates tool_result text during conversion (0 disables).
	MaxToolResultChars int
}

// Anthropic message request (subset used by Claude Code).
type anthropicMessagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []anthropicMsg  `json:"messages"`
	Tools         []anthropicTool `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	StopSequences []string        `json:"stop_sequences"`
	Metadata      json.RawMessage `json:"metadata"`
	// Thinking controls are translated to Responses reasoning.effort.
	Thinking     json.RawMessage `json:"thinking"`
	OutputConfig json.RawMessage `json:"output_config"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Type        string          `json:"type,omitempty"`
}

// Responses-ish request body.
type responsesRequest struct {
	Model             string               `json:"model"`
	Instructions      string               `json:"instructions,omitempty"`
	Input             []map[string]any     `json:"input"`
	Tools             []map[string]any     `json:"tools,omitempty"`
	ToolChoice        any                  `json:"tool_choice,omitempty"`
	Reasoning         *responsesReasoning  `json:"reasoning,omitempty"`
	Text              *responsesTextConfig `json:"text,omitempty"`
	Include           []string             `json:"include,omitempty"`
	MaxOutputTokens   int                  `json:"max_output_tokens,omitempty"`
	Stream            bool                 `json:"stream"`
	Temperature       *float64             `json:"temperature,omitempty"`
	TopP              *float64             `json:"top_p,omitempty"`
	PromptCacheKey    string               `json:"prompt_cache_key,omitempty"`
	ParallelToolCalls *bool                `json:"parallel_tool_calls,omitempty"`
}

// TranslateRequest converts Anthropic Messages JSON → Grok Responses-ish JSON.
// Returns the marshaled body and the original request model string (for response echo).
func TranslateRequest(raw []byte, opts TranslateReqOptions) (body []byte, originalModel string, stream bool, err error) {
	var req anthropicMessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, "", false, fmt.Errorf("anthropic: invalid messages body: %w", err)
	}
	originalModel = strings.TrimSpace(req.Model)
	model := strings.TrimSpace(opts.ResolvedModel)
	if model == "" {
		model = originalModel
	}

	out := responsesRequest{
		Model:  model,
		Input:  make([]map[string]any, 0, len(req.Messages)+1),
		Stream: req.Stream,
	}
	if req.MaxTokens > 0 {
		out.MaxOutputTokens = req.MaxTokens
	}
	// Grok Build reasoning models reject top_k and stop sequences. Claude Code
	// may still send these compatibility hints, so consume them locally rather
	// than turning otherwise valid subrequests into 400 responses.
	out.Temperature = req.Temperature
	out.TopP = req.TopP
	// Anthropic metadata is attribution-only. Grok Build Responses rejects
	// this field, so consume it without forwarding.
	if opts.ConvID != "" {
		out.PromptCacheKey = opts.ConvID
	}

	// system → instructions
	if len(req.System) > 0 && string(req.System) != "null" {
		out.Instructions = extractSystemText(req.System)
	}
	out.Reasoning, err = translateThinkingConfig(
		req.Thinking,
		req.OutputConfig,
		model,
		opts.StripUnknownBetas,
		req.MaxTokens,
		len(req.Tools) > 0,
	)
	if err != nil {
		return nil, originalModel, false, err
	}
	out.Text, err = translateOutputFormat(req.OutputConfig)
	if err != nil {
		return nil, originalModel, false, err
	}
	if thinkingBridgeFromRaw(req.Thinking).Enabled {
		out.Include = []string{"reasoning.encrypted_content"}
	}

	// messages → input
	for _, m := range req.Messages {
		items, convErr := convertMessage(m, opts)
		if convErr != nil {
			return nil, originalModel, false, convErr
		}
		out.Input = append(out.Input, items...)
	}

	// tools
	hasServerWebSearch := false
	if len(req.Tools) > 0 {
		out.Tools = make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if isAnthropicWebSearchTool(t.Type) {
				hasServerWebSearch = true
				out.Tools = append(out.Tools, map[string]any{"type": "web_search"})
				continue
			}
			params := json.RawMessage(`{}`)
			if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
				params = t.InputSchema
			}
			// Drop $schema if present (Grok sometimes rejects it).
			params = stripJSONField(params, "$schema")
			tool := map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  json.RawMessage(params),
			}
			out.Tools = append(out.Tools, tool)
		}
	}

	// tool_choice (best-effort)
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		if hasServerWebSearch && toolChoiceForces(req.ToolChoice, "web_search") {
			// xAI cannot force a specific provider-hosted tool. Dropping the
			// forced choice lets the model execute the mapped built-in search.
			out.ToolChoice = "auto"
		} else {
			out.ToolChoice = convertToolChoice(req.ToolChoice)
		}
		var tc struct {
			DisableParallel *bool `json:"disable_parallel_tool_use"`
		}
		if json.Unmarshal(req.ToolChoice, &tc) == nil && tc.DisableParallel != nil {
			v := !*tc.DisableParallel
			out.ParallelToolCalls = &v
		}
	}

	body, err = json.Marshal(out)
	if err != nil {
		return nil, originalModel, false, fmt.Errorf("anthropic: marshal responses body: %w", err)
	}
	return body, originalModel, req.Stream, nil
}

func isAnthropicWebSearchTool(toolType string) bool {
	toolType = strings.ToLower(strings.TrimSpace(toolType))
	return toolType == "web_search" || strings.HasPrefix(toolType, "web_search_")
}

func toolChoiceForces(raw json.RawMessage, name string) bool {
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &choice) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(choice.Type), "tool") &&
		strings.EqualFold(strings.TrimSpace(choice.Name), strings.TrimSpace(name))
}

func extractSystemText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "" || bl.Type == "text" {
				if bl.Text == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return ""
}

func convertMessage(m anthropicMsg, opts TranslateReqOptions) ([]map[string]any, error) {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		role = "user"
	}

	// content as plain string
	var text string
	if err := json.Unmarshal(m.Content, &text); err == nil {
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		return []map[string]any{
			{
				"type": "message",
				"role": role,
				"content": []map[string]any{
					{"type": partType, "text": text},
				},
			},
		}, nil
	}

	// content as array of blocks
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return []map[string]any{
			{
				"type": "message",
				"role": role,
				"content": []map[string]any{
					{"type": "input_text", "text": string(m.Content)},
				},
			},
		}, nil
	}

	var out []map[string]any
	var textParts []map[string]any
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		out = append(out, map[string]any{
			"type":    "message",
			"role":    role,
			"content": textParts,
		})
		textParts = nil
	}

	for _, bl := range blocks {
		typ := rawString(bl["type"])
		switch typ {
		case "text", "":
			t := rawString(bl["text"])
			if opts.PreserveCacheHints {
				if hint := cacheControlHint(bl["cache_control"]); hint != "" && t != "" {
					t = t + "\n<!-- " + hint + " -->"
				}
			}
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			textParts = append(textParts, map[string]any{"type": partType, "text": t})
		case "image":
			var source struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
				URL       string `json:"url"`
			}
			_ = json.Unmarshal(bl["source"], &source)
			url := source.URL
			if url == "" && source.Data != "" {
				mt := source.MediaType
				if mt == "" {
					mt = "application/octet-stream"
				}
				url = "data:" + mt + ";base64," + source.Data
			}
			if url != "" {
				textParts = append(textParts, map[string]any{
					"type":      "input_image",
					"image_url": url,
				})
			}
		case "tool_use":
			flushText()
			id := rawString(bl["id"])
			name := rawString(bl["name"])
			args := bl["input"]
			argsStr := "{}"
			if len(args) > 0 && string(args) != "null" {
				if json.Valid(args) {
					argsStr = string(args)
				} else {
					b, _ := json.Marshal(string(args))
					argsStr = string(b)
				}
			}
			out = append(out, map[string]any{
				"type":      "function_call",
				"call_id":   id,
				"id":        id,
				"name":      name,
				"arguments": argsStr,
			})
		case "tool_result":
			flushText()
			callID := rawString(bl["tool_use_id"])
			if callID == "" {
				callID = rawString(bl["id"])
			}
			output := toolResultOutput(bl["content"])
			if opts.MaxToolResultChars > 0 {
				output = TruncateToolResultText(output, opts.MaxToolResultChars)
			}
			if rawBool(bl["is_error"]) {
				encoded, _ := json.Marshal(map[string]any{"is_error": true, "content": output})
				output = string(encoded)
			}
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  output,
			})
		case "thinking":
			signature := rawString(bl["signature"])
			if signature == "" {
				continue
			}
			flushText()
			out = append(out, map[string]any{
				"type":              "reasoning",
				"summary":           []any{},
				"encrypted_content": signature,
			})
		case "redacted_thinking":
			// Safety-redacted Anthropic payloads are not Grok encrypted
			// reasoning and cannot be replayed across providers.
			continue
		default:
			return nil, fmt.Errorf("unsupported Anthropic content block type %q", typ)
		}
	}
	flushText()
	return out, nil
}

func toolResultOutput(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, bl := range blocks {
			if bl.Type == "text" || bl.Type == "" {
				if bl.Text == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(bl.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func convertToolChoice(raw json.RawMessage) any {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "any":
			return "required"
		case "auto", "none", "required":
			return s
		default:
			return s
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := obj["name"].(string)
		return map[string]any{
			"type": "function",
			"name": name,
		}
	default:
		return obj
	}
}

func rawString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	return strings.Trim(string(r), `"`)
}

func rawBool(r json.RawMessage) bool {
	var value bool
	return json.Unmarshal(r, &value) == nil && value
}

func stripJSONField(raw json.RawMessage, field string) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if _, ok := m[field]; !ok {
		return raw
	}
	delete(m, field)
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

func cacheControlHint(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return "cache_control"
	}
	if strings.TrimSpace(obj.Type) == "" {
		return "cache_control"
	}
	return "cache_control:" + strings.TrimSpace(obj.Type)
}
