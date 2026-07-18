package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// ContextGuardConfig controls local token estimation, auto-compact and protection.
type ContextGuardConfig struct {
	MaxInputTokens     int
	SoftInputTokens    int
	MaxToolResultChars int
	KeepRecentMessages int
	PreserveCacheHints bool
	AutoCompact        bool
}

// CompactResult describes what auto-compact did.
type CompactResult struct {
	Applied              bool
	EstimatedBefore      int
	EstimatedAfter       int
	DroppedMessages      int
	TruncatedToolResults int
	Note                 string
}

// EstimateTokens estimates input tokens for Claude-like payloads.
func EstimateTokens(value any) int {
	tokens := estimateValueTokens(value)
	if tokens < 1 {
		return 1
	}
	return tokens
}

func estimateValueTokens(value any) int {
	switch v := value.(type) {
	case string:
		return estimateStringTokens(v)
	case json.Number:
		return estimateStringTokens(v.String())
	case float64:
		return 1
	case bool:
		return 1
	case nil:
		return 0
	case []any:
		total := 0
		for _, item := range v {
			total += estimateValueTokens(item)
		}
		if len(v) > 0 {
			total += len(v)
		}
		return total
	case map[string]any:
		total := 0
		for key, item := range v {
			switch key {
			case "model", "stream", "temperature", "top_p", "top_k", "n", "logprobs":
				continue
			case "cache_control":
				total += 2
				continue
			}
			total += estimateStringTokens(key) / 4
			total += estimateValueTokens(item)
		}
		return total
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return 0
		}
		return estimateStringTokens(string(b))
	}
}

func estimateStringTokens(s string) int {
	if s == "" {
		return 0
	}
	var ascii, cjk, other, ws int
	for _, r := range s {
		switch {
		case r == ' ' || r == '\n' || r == '\t' || r == '\r':
			ws++
		case r <= unicode.MaxASCII:
			ascii++
		case unicode.In(r, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana):
			cjk++
		default:
			other++
		}
	}
	tokens := float64(ascii)/4.0 + float64(ws)/6.0 + float64(cjk)/1.5 + float64(other)/2.5 + 1.5
	if tokens < 1 {
		return 1
	}
	return int(tokens + 0.999)
}

// EstimateRawTokens estimates tokens from a JSON body with a conservative byte floor.
func EstimateRawTokens(raw []byte) int {
	if len(raw) == 0 {
		return 1
	}
	var value any
	tokens := 0
	if err := json.Unmarshal(raw, &value); err == nil {
		tokens = EstimateTokens(value)
	} else {
		tokens = estimateStringTokens(string(raw))
	}
	// Soft floor only — too aggressive floors (e.g. bytes/3) make normal Claude Code
	// sessions look "over budget" and trigger history-dropping auto-compact loops.
	byteFloor := (len(raw) + 3) / 4
	if byteFloor > tokens {
		tokens = byteFloor
	}
	if tokens < 1 {
		return 1
	}
	return tokens
}

// PrepareAnthropicBody runs tool truncation + auto-compact + hard guard for Anthropic Messages bodies.
func PrepareAnthropicBody(raw []byte, cfg ContextGuardConfig) (out []byte, result CompactResult, err error) {
	return prepareBody(raw, cfg, bodyShapeAnthropic)
}

// PrepareOpenAIBody runs the same guard for OpenAI chat.completions or Responses bodies.
// It accepts either messages[] (chat) or input[] (Responses).
func PrepareOpenAIBody(raw []byte, cfg ContextGuardConfig) (out []byte, result CompactResult, err error) {
	return prepareBody(raw, cfg, bodyShapeOpenAI)
}

type bodyShape int

const (
	bodyShapeAnthropic bodyShape = iota
	bodyShapeOpenAI
)

func prepareBody(raw []byte, cfg ContextGuardConfig, shape bodyShape) (out []byte, result CompactResult, err error) {
	before := EstimateRawTokens(raw)
	result.EstimatedBefore = before
	out = append([]byte(nil), raw...)

	// Leave headroom under the common 500k upstream hard limit.
	// Prefer preserving Claude Code history over aggressive compact — dropping
	// mid-task tool turns causes the agent to re-read the same files in a loop.
	safetyTarget := cfg.SoftInputTokens
	if safetyTarget <= 0 || safetyTarget > 470000 {
		safetyTarget = 470000
	}
	if cfg.MaxInputTokens > 0 && cfg.MaxInputTokens < safetyTarget {
		safetyTarget = cfg.MaxInputTokens
	}

	toolBudget := cfg.MaxToolResultChars
	if toolBudget <= 0 {
		toolBudget = 120000
	}

	// Always truncate oversized tool results first (cheap and high impact).
	if rewritten, n, terr := truncateToolResultsInBody(out, toolBudget); terr == nil && n > 0 {
		out = rewritten
		result.TruncatedToolResults += n
		result.Applied = true
	}

	if cfg.AutoCompact {
		keep := cfg.KeepRecentMessages
		if keep <= 0 {
			keep = 16
		}
		for attempt := 0; attempt < 12 && EstimateRawTokens(out) > safetyTarget; attempt++ {
			rewritten, dropped, note, cerr := compactMessagesBody(out, keep)
			if cerr == nil && (dropped > 0 || len(rewritten) != len(out)) {
				out = rewritten
				result.DroppedMessages += dropped
				result.Note = note
				result.Applied = true
			}

			if toolBudget > 3000 {
				toolBudget = toolBudget * 2 / 3
				if toolBudget < 3000 {
					toolBudget = 3000
				}
			}
			if rewritten, n, terr := truncateToolResultsInBody(out, toolBudget); terr == nil && n > 0 {
				out = rewritten
				result.TruncatedToolResults += n
				result.Applied = true
			}
			if rewritten, n, terr := truncateLargeTextBlocks(out, toolBudget); terr == nil && n > 0 {
				out = rewritten
				result.Applied = true
			}

			// Do not shrink tool schemas / system prompts / keep=1 here.
			// Those destroy Claude Code agent state and trigger tool-read loops.
			if keep > 2 {
				next := keep * 2 / 3
				if next < 2 {
					next = 2
				}
				keep = next
			}
			_ = shape
		}
	} else {
		// Even without auto-compact, still clamp giant tool outputs once.
		if rewritten, n, terr := truncateLargeTextBlocks(out, toolBudget); terr == nil && n > 0 {
			out = rewritten
			result.Applied = true
		}
	}

	// Emergency pass: recent messages, tool schemas, or the system prompt can
	// individually exceed the hard limit even after normal compaction. Prefer a
	// degraded but usable request over an otherwise guaranteed 400 response.
	if cfg.AutoCompact && cfg.MaxInputTokens > 0 && EstimateRawTokens(out) > cfg.MaxInputTokens {
		for attempt := 0; attempt < 3 && EstimateRawTokens(out) > cfg.MaxInputTokens; attempt++ {
			if rewritten, n, terr := truncateToolsInBody(out, 6000); terr == nil && n > 0 {
				out = rewritten
				result.Applied = true
			}
		}
		if EstimateRawTokens(out) > cfg.MaxInputTokens {
			if rewritten, n, terr := truncateInstructionsInBody(out, 24000); terr == nil && n > 0 {
				out = rewritten
				result.Applied = true
			}
		}
		if EstimateRawTokens(out) > cfg.MaxInputTokens {
			if rewritten, dropped, note, cerr := compactMessagesBody(out, 1); cerr == nil && dropped > 0 {
				out = rewritten
				result.DroppedMessages += dropped
				result.Note = note
				result.Applied = true
			}
		}
		if EstimateRawTokens(out) > cfg.MaxInputTokens {
			if rewritten, n, terr := truncateLargeTextBlocks(out, 3000); terr == nil && n > 0 {
				out = rewritten
				result.Applied = true
			}
		}
	}

	after := EstimateRawTokens(out)
	result.EstimatedAfter = after
	if cfg.MaxInputTokens > 0 && after > cfg.MaxInputTokens {
		breakdown := contextSizeBreakdown(out)
		return out, result, fmt.Errorf(
			"context_too_long: estimated input_tokens=%d exceeds max_input_tokens=%d after auto-compact (%s); start a new session or remove the largest component",
			after, cfg.MaxInputTokens, breakdown,
		)
	}
	return out, result, nil
}

func contextSizeBreakdown(raw []byte) string {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return fmt.Sprintf("request_bytes=%d", len(raw))
	}
	parts := make([]string, 0, 4)
	for _, key := range []string{"system", "instructions", "tools", "messages", "input"} {
		if value, ok := root[key]; ok {
			parts = append(parts, fmt.Sprintf("%s_bytes=%d", key, len(value)))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("request_bytes=%d", len(raw))
	}
	return strings.Join(parts, ", ")
}

func messageArrayKey(root map[string]json.RawMessage) string {
	if _, ok := root["messages"]; ok {
		return "messages"
	}
	if _, ok := root["input"]; ok {
		return "input"
	}
	return ""
}

func truncateToolResultsInBody(raw []byte, maxChars int) ([]byte, int, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, 0, err
	}
	key := messageArrayKey(root)
	if key == "" {
		return raw, 0, nil
	}
	msgRaw := root[key]
	if len(msgRaw) == 0 {
		return raw, 0, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &messages); err != nil {
		return raw, 0, err
	}
	changed := 0
	for i := range messages {
		// OpenAI tool role messages: role=tool, content=string|array
		if role := rawString(messages[i]["role"]); role == "tool" || role == "function" {
			if contentRaw, ok := messages[i]["content"]; ok {
				if newContent, did, err := truncateToolResultContent(contentRaw, maxChars); err == nil && did {
					messages[i]["content"] = newContent
					changed++
				}
			}
			continue
		}
		// OpenAI Responses function_call_output items.
		if typ := rawString(messages[i]["type"]); typ == "function_call_output" || typ == "tool_result" {
			if outRaw, ok := messages[i]["output"]; ok {
				if newOut, did, err := truncateToolResultContent(outRaw, maxChars); err == nil && did {
					messages[i]["output"] = newOut
					changed++
				}
			}
			if contentRaw, ok := messages[i]["content"]; ok {
				if newContent, did, err := truncateToolResultContent(contentRaw, maxChars); err == nil && did {
					messages[i]["content"] = newContent
					changed++
				}
			}
			continue
		}

		contentRaw, ok := messages[i]["content"]
		if !ok {
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(contentRaw, &blocks); err != nil {
			var s string
			if json.Unmarshal(contentRaw, &s) == nil && len(s) > maxChars {
				// Only auto-truncate plain string content when role looks tool-like,
				// otherwise leave to truncateLargeTextBlocks.
				continue
			}
			continue
		}
		blockChanged := false
		for j := range blocks {
			typ := rawString(blocks[j]["type"])
			if typ != "tool_result" && typ != "function_call_output" {
				// OpenAI chat tool_calls are not results; skip.
				// Anthropic tool_result lives in content blocks.
				continue
			}
			newContent, did, err := truncateToolResultContent(blocks[j]["content"], maxChars)
			if err != nil || !did {
				// Some shapes put payload in "output".
				if outRaw, ok := blocks[j]["output"]; ok {
					if newOut, did2, err2 := truncateToolResultContent(outRaw, maxChars); err2 == nil && did2 {
						blocks[j]["output"] = newOut
						blockChanged = true
						changed++
					}
				}
				continue
			}
			blocks[j]["content"] = newContent
			blockChanged = true
			changed++
		}
		if blockChanged {
			b, err := json.Marshal(blocks)
			if err != nil {
				return raw, 0, err
			}
			messages[i]["content"] = b
		}
	}
	if changed == 0 {
		return raw, 0, nil
	}
	mb, err := json.Marshal(messages)
	if err != nil {
		return raw, 0, err
	}
	root[key] = mb
	out, err := json.Marshal(root)
	if err != nil {
		return raw, 0, err
	}
	return out, changed, nil
}

func truncateLargeTextBlocks(raw []byte, maxChars int) ([]byte, int, error) {
	if maxChars <= 0 {
		return raw, 0, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, 0, err
	}
	changed := 0
	for _, field := range []string{"system", "instructions"} {
		if sys, ok := root[field]; ok {
			if ns, did, err := truncateAnyTextField(sys, maxChars); err == nil && did {
				root[field] = ns
				changed++
			}
		}
	}
	key := messageArrayKey(root)
	if key == "" {
		if changed == 0 {
			return raw, 0, nil
		}
		out, err := json.Marshal(root)
		return out, changed, err
	}
	msgRaw := root[key]
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &messages); err != nil {
		return raw, changed, err
	}
	for i := range messages {
		// Prefer truncating bulky assistant/user text; always clamp tool payloads.
		role := rawString(messages[i]["role"])
		typ := rawString(messages[i]["type"])
		isToolish := role == "tool" || role == "function" || typ == "function_call_output" || typ == "tool_result"

		if outRaw, ok := messages[i]["output"]; ok && isToolish {
			if ns, did, err := truncateAnyTextField(outRaw, maxChars); err == nil && did {
				messages[i]["output"] = ns
				changed++
			}
		}

		contentRaw, ok := messages[i]["content"]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(contentRaw, &s) == nil {
			limit := maxChars
			if !isToolish {
				// Keep user/assistant plain text larger than tool dumps.
				limit = maxChars * 2
				if limit < maxChars {
					limit = maxChars
				}
			}
			if len(s) > limit {
				b, err := json.Marshal(TruncateToolResultText(s, limit))
				if err == nil {
					messages[i]["content"] = b
					changed++
				}
			}
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(contentRaw, &blocks); err != nil {
			continue
		}
		blockChanged := false
		for j := range blocks {
			btype := rawString(blocks[j]["type"])
			if btype == "tool_result" || btype == "function_call_output" {
				if newContent, did, err := truncateToolResultContent(blocks[j]["content"], maxChars); err == nil && did {
					blocks[j]["content"] = newContent
					blockChanged = true
					changed++
				}
				if outRaw, ok := blocks[j]["output"]; ok {
					if newOut, did, err := truncateToolResultContent(outRaw, maxChars); err == nil && did {
						blocks[j]["output"] = newOut
						blockChanged = true
						changed++
					}
				}
				continue
			}
			if btype != "text" && btype != "input_text" && btype != "output_text" && btype != "" {
				continue
			}
			text := rawString(blocks[j]["text"])
			if text == "" {
				// OpenAI content parts sometimes use "content" instead of "text".
				text = rawString(blocks[j]["content"])
				if text == "" || len(text) <= maxChars*2 {
					continue
				}
				b, err := json.Marshal(TruncateToolResultText(text, maxChars*2))
				if err != nil {
					continue
				}
				blocks[j]["content"] = b
				blockChanged = true
				changed++
				continue
			}
			limit := maxChars * 2
			if len(text) <= limit {
				continue
			}
			b, err := json.Marshal(TruncateToolResultText(text, limit))
			if err != nil {
				continue
			}
			blocks[j]["text"] = b
			blockChanged = true
			changed++
		}
		if blockChanged {
			b, err := json.Marshal(blocks)
			if err == nil {
				messages[i]["content"] = b
			}
		}
	}
	if changed == 0 {
		return raw, 0, nil
	}
	mb, err := json.Marshal(messages)
	if err != nil {
		return raw, 0, err
	}
	root[key] = mb
	out, err := json.Marshal(root)
	return out, changed, err
}

func truncateToolsInBody(raw []byte, maxChars int) ([]byte, int, error) {
	if maxChars <= 0 {
		return raw, 0, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, 0, err
	}
	toolsRaw, ok := root["tools"]
	if !ok || len(toolsRaw) == 0 {
		return raw, 0, nil
	}
	var tools []map[string]any
	if err := json.Unmarshal(toolsRaw, &tools); err != nil {
		return raw, 0, err
	}
	if len(tools) == 0 {
		return raw, 0, nil
	}
	changed := 0
	// Keep tool names; shrink descriptions and parameters dumps.
	descLimit := maxChars / 4
	if descLimit < 200 {
		descLimit = 200
	}
	if descLimit > 4000 {
		descLimit = 4000
	}
	for i := range tools {
		if desc, ok := tools[i]["description"].(string); ok && len(desc) > descLimit {
			tools[i]["description"] = TruncateToolResultText(desc, descLimit)
			changed++
		}
		// OpenAI nested function object.
		if fn, ok := tools[i]["function"].(map[string]any); ok {
			if desc, ok := fn["description"].(string); ok && len(desc) > descLimit {
				fn["description"] = TruncateToolResultText(desc, descLimit)
				tools[i]["function"] = fn
				changed++
			}
			// Drop huge parameter schema bodies when over budget by replacing with minimal object schema.
			if params, ok := fn["parameters"]; ok {
				pb, _ := json.Marshal(params)
				if len(pb) > maxChars {
					fn["parameters"] = map[string]any{"type": "object", "additionalProperties": true}
					tools[i]["function"] = fn
					changed++
				}
			}
		}
		if params, ok := tools[i]["parameters"]; ok {
			pb, _ := json.Marshal(params)
			if len(pb) > maxChars {
				tools[i]["parameters"] = map[string]any{"type": "object", "additionalProperties": true}
				changed++
			}
		}
		if schema, ok := tools[i]["input_schema"]; ok {
			pb, _ := json.Marshal(schema)
			if len(pb) > maxChars {
				tools[i]["input_schema"] = map[string]any{"type": "object", "additionalProperties": true}
				changed++
			}
		}
	}
	// If still many tools, drop the oldest half of definitions (keep last N).
	if len(tools) > 32 {
		keep := len(tools) / 2
		if keep < 16 {
			keep = 16
		}
		if keep < len(tools) {
			tools = tools[len(tools)-keep:]
			changed++
		}
	}
	if changed == 0 {
		return raw, 0, nil
	}
	tb, err := json.Marshal(tools)
	if err != nil {
		return raw, 0, err
	}
	root["tools"] = tb
	out, err := json.Marshal(root)
	return out, changed, err
}

func truncateInstructionsInBody(raw []byte, maxChars int) ([]byte, int, error) {
	if maxChars <= 0 {
		return raw, 0, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, 0, err
	}
	changed := 0
	// System/instructions often dominate Claude Code long sessions.
	limit := maxChars
	if limit < 4000 {
		limit = 4000
	}
	for _, field := range []string{"system", "instructions"} {
		if val, ok := root[field]; ok {
			if ns, did, err := truncateAnyTextField(val, limit); err == nil && did {
				root[field] = ns
				changed++
			}
		}
	}
	if changed == 0 {
		return raw, 0, nil
	}
	out, err := json.Marshal(root)
	return out, changed, err
}

func truncateAnyTextField(raw json.RawMessage, maxChars int) (json.RawMessage, bool, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		ns := TruncateToolResultText(s, maxChars)
		if ns == s {
			return raw, false, nil
		}
		b, err := json.Marshal(ns)
		return b, true, err
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return raw, false, nil
	}
	changed := false
	for i := range blocks {
		text, _ := blocks[i]["text"].(string)
		if text == "" {
			if c, ok := blocks[i]["content"].(string); ok {
				text = c
				if text == "" || len(text) <= maxChars {
					continue
				}
				blocks[i]["content"] = TruncateToolResultText(text, maxChars)
				changed = true
			}
			continue
		}
		if len(text) <= maxChars {
			continue
		}
		blocks[i]["text"] = TruncateToolResultText(text, maxChars)
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	b, err := json.Marshal(blocks)
	return b, true, err
}

func truncateToolResultContent(raw json.RawMessage, maxChars int) (json.RawMessage, bool, error) {
	if len(raw) == 0 || maxChars <= 0 {
		return raw, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		ns := TruncateToolResultText(s, maxChars)
		if ns == s {
			return raw, false, nil
		}
		b, err := json.Marshal(ns)
		return b, true, err
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		if len(raw) <= maxChars {
			return raw, false, nil
		}
		b, err := json.Marshal(TruncateToolResultText(string(raw), maxChars))
		return b, true, err
	}
	changed := false
	for i := range blocks {
		text, _ := blocks[i]["text"].(string)
		if text == "" {
			if c, ok := blocks[i]["content"].(string); ok && c != "" {
				nt := TruncateToolResultText(c, maxChars)
				if nt != c {
					blocks[i]["content"] = nt
					changed = true
				}
			}
			continue
		}
		nt := TruncateToolResultText(text, maxChars)
		if nt != text {
			blocks[i]["text"] = nt
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	b, err := json.Marshal(blocks)
	return b, true, err
}

func compactMessagesBody(raw []byte, keep int) ([]byte, int, string, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, 0, "", err
	}
	key := messageArrayKey(root)
	if key == "" {
		return raw, 0, "", nil
	}
	msgRaw := root[key]
	var messages []json.RawMessage
	if err := json.Unmarshal(msgRaw, &messages); err != nil {
		return raw, 0, "", err
	}
	if keep <= 0 {
		keep = 16
	}
	if len(messages) <= keep {
		return raw, 0, "", nil
	}
	start := len(messages) - keep
	// Prefer not to cut in the middle of a tool_use/tool_result pair when possible.
	start = alignCompactStart(messages, start)
	kept := append([]json.RawMessage(nil), messages[start:]...)
	dropped := start
	noticeText := fmt.Sprintf("[context auto-compacted by grokbuild-proxy: dropped %d older messages; recent turns preserved]", dropped)
	var notice map[string]any
	if key == "input" {
		// Responses input item.
		notice = map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": noticeText,
			}},
		}
	} else {
		// Anthropic / OpenAI chat message.
		notice = map[string]any{
			"role": "user",
			"content": []map[string]any{{
				"type": "text",
				"text": noticeText,
			}},
		}
	}
	nb, err := json.Marshal(notice)
	if err != nil {
		return raw, 0, "", err
	}
	finalMsgs := make([]json.RawMessage, 0, len(kept)+1)
	finalMsgs = append(finalMsgs, nb)
	finalMsgs = append(finalMsgs, kept...)
	mb, err := json.Marshal(finalMsgs)
	if err != nil {
		return raw, 0, "", err
	}
	root[key] = mb
	out, err := json.Marshal(root)
	if err != nil {
		return raw, 0, "", err
	}
	return out, dropped, fmt.Sprintf("auto-compact dropped %d older messages", dropped), nil
}

func alignCompactStart(messages []json.RawMessage, start int) int {
	if start <= 0 || start >= len(messages) {
		return start
	}
	// If the first kept message is a tool result / tool role, walk back to include its parent assistant call.
	for start > 0 && start < len(messages) {
		var probe map[string]any
		if json.Unmarshal(messages[start], &probe) != nil {
			break
		}
		role, _ := probe["role"].(string)
		typ, _ := probe["type"].(string)
		if role == "tool" || role == "function" || typ == "function_call_output" || typ == "tool_result" {
			start--
			continue
		}
		// Anthropic user message that only contains tool_result blocks.
		if role == "user" {
			if content, ok := probe["content"].([]any); ok && len(content) > 0 {
				onlyTool := true
				for _, c := range content {
					m, ok := c.(map[string]any)
					if !ok {
						onlyTool = false
						break
					}
					t, _ := m["type"].(string)
					if t != "tool_result" {
						onlyTool = false
						break
					}
				}
				if onlyTool {
					start--
					continue
				}
			}
		}
		break
	}
	return start
}

// TruncateToolResultText trims oversized tool outputs while preserving head/tail.
func TruncateToolResultText(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	if maxChars < 64 {
		return string(runes[:maxChars]) + "…"
	}
	head := maxChars * 2 / 3
	tail := maxChars - head - 48
	if tail < 16 {
		tail = 16
		head = maxChars - tail - 48
	}
	if head < 16 {
		head = maxChars / 2
		tail = maxChars - head - 24
	}
	if head+tail+24 > len(runes) {
		return string(runes[:maxChars]) + "…"
	}
	omitted := len(runes) - head - tail
	var b strings.Builder
	b.Grow(maxChars + 80)
	b.WriteString(string(runes[:head]))
	b.WriteString("\n\n...[truncated ")
	b.WriteString(fmt.Sprintf("%d", omitted))
	b.WriteString(" chars of tool_result]...\n\n")
	b.WriteString(string(runes[len(runes)-tail:]))
	return b.String()
}

// IsContextTooLongMessage detects upstream/context overflow errors.
func IsContextTooLongMessage(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	needles := []string{
		"maximum prompt length",
		"context length",
		"context_window",
		"context window",
		"too many tokens",
		"prompt is too long",
		"prompt too long",
		"input is too long",
		"context_too_long",
		"please reduce the length",
		"exceeds the context",
		"token limit",
		"invalid-argument",
		"request contains",
	}
	for _, n := range needles {
		if strings.Contains(m, n) {
			return true
		}
	}
	return false
}
