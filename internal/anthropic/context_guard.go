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
			case "model", "stream", "temperature", "top_p", "top_k":
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
	// Conservative floor for code/log-heavy payloads.
	byteFloor := (len(raw) + 2) / 3
	if byteFloor > tokens {
		tokens = byteFloor
	}
	if tokens < 1 {
		return 1
	}
	return tokens
}

// PrepareAnthropicBody runs tool truncation + auto-compact + hard guard.
func PrepareAnthropicBody(raw []byte, cfg ContextGuardConfig) (out []byte, result CompactResult, err error) {
	before := EstimateRawTokens(raw)
	result.EstimatedBefore = before
	out = append([]byte(nil), raw...)

	// Always leave headroom under common 500k upstream limit.
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
			if keep > 2 {
				next := keep * 2 / 3
				if next < 2 {
					next = 2
				}
				keep = next
			}
		}
	}

	after := EstimateRawTokens(out)
	result.EstimatedAfter = after
	if cfg.MaxInputTokens > 0 && after > cfg.MaxInputTokens {
		return out, result, fmt.Errorf(
			"context_too_long: estimated input_tokens=%d exceeds max_input_tokens=%d after auto-compact; start a new session or remove large tool outputs",
			after, cfg.MaxInputTokens,
		)
	}
	return out, result, nil
}

func truncateToolResultsInBody(raw []byte, maxChars int) ([]byte, int, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, 0, err
	}
	msgRaw, ok := root["messages"]
	if !ok || len(msgRaw) == 0 {
		return raw, 0, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &messages); err != nil {
		return raw, 0, err
	}
	changed := 0
	for i := range messages {
		contentRaw, ok := messages[i]["content"]
		if !ok {
			continue
		}
		var blocks []map[string]json.RawMessage
		if err := json.Unmarshal(contentRaw, &blocks); err != nil {
			var s string
			if json.Unmarshal(contentRaw, &s) == nil && len(s) > maxChars {
				ns := TruncateToolResultText(s, maxChars)
				b, err := json.Marshal(ns)
				if err == nil {
					messages[i]["content"] = b
					changed++
				}
			}
			continue
		}
		blockChanged := false
		for j := range blocks {
			if rawString(blocks[j]["type"]) != "tool_result" {
				continue
			}
			newContent, did, err := truncateToolResultContent(blocks[j]["content"], maxChars)
			if err != nil || !did {
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
	root["messages"] = mb
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
	if sys, ok := root["system"]; ok {
		if ns, did, err := truncateAnyTextField(sys, maxChars); err == nil && did {
			root["system"] = ns
			changed++
		}
	}
	msgRaw, ok := root["messages"]
	if !ok {
		if changed == 0 {
			return raw, 0, nil
		}
		out, err := json.Marshal(root)
		return out, changed, err
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(msgRaw, &messages); err != nil {
		return raw, changed, err
	}
	for i := range messages {
		contentRaw, ok := messages[i]["content"]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(contentRaw, &s) == nil {
			if len(s) > maxChars {
				b, err := json.Marshal(TruncateToolResultText(s, maxChars))
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
			typ := rawString(blocks[j]["type"])
			if typ != "text" && typ != "" {
				continue
			}
			text := rawString(blocks[j]["text"])
			if len(text) <= maxChars {
				continue
			}
			b, err := json.Marshal(TruncateToolResultText(text, maxChars))
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
	root["messages"] = mb
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
		if text == "" || len(text) <= maxChars {
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
	msgRaw, ok := root["messages"]
	if !ok {
		return raw, 0, "", nil
	}
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
	kept := append([]json.RawMessage(nil), messages[start:]...)
	dropped := start
	notice := map[string]any{
		"role": "user",
		"content": []map[string]any{{
			"type": "text",
			"text": fmt.Sprintf("[context auto-compacted by grokbuild-proxy: dropped %d older messages; recent turns preserved]", dropped),
		}},
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
	root["messages"] = mb
	out, err := json.Marshal(root)
	if err != nil {
		return raw, 0, "", err
	}
	return out, dropped, fmt.Sprintf("auto-compact dropped %d older messages", dropped), nil
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
	}
	for _, n := range needles {
		if strings.Contains(m, n) {
			return true
		}
	}
	return false
}
