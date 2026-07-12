package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAutoCompactDropsOldMessages(t *testing.T) {
	msgs := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		msgs = append(msgs, map[string]any{
			"role": "user",
			"content": "message " + strings.Repeat("x", 2000) + " " + string(rune('a'+(i%26))),
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4",
		"messages": msgs,
		"max_tokens": 16,
	})
	cfg := ContextGuardConfig{
		AutoCompact:        true,
		SoftInputTokens:    1000,
		MaxInputTokens:     500000,
		MaxToolResultChars: 500,
		KeepRecentMessages: 8,
	}
	out, res, err := PrepareAnthropicBody(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.DroppedMessages <= 0 {
		t.Fatalf("expected compact, got %+v", res)
	}
	if res.EstimatedAfter >= res.EstimatedBefore {
		t.Fatalf("expected smaller after compact: before=%d after=%d", res.EstimatedBefore, res.EstimatedAfter)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	arr, _ := parsed["messages"].([]any)
	if len(arr) == 0 || len(arr) > 12 {
		t.Fatalf("unexpected message count %d", len(arr))
	}
}

func TestToolResultTruncate(t *testing.T) {
	big := strings.Repeat("tool-output-", 20000)
	body, _ := json.Marshal(map[string]any{
		"model": "claude-sonnet-4",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type": "tool_result",
				"tool_use_id": "t1",
				"content": big,
			}},
		}},
	})
	cfg := ContextGuardConfig{MaxToolResultChars: 1000, AutoCompact: false, MaxInputTokens: 0}
	out, res, err := PrepareAnthropicBody(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.TruncatedToolResults < 1 {
		t.Fatalf("expected truncation: %+v", res)
	}
	if !strings.Contains(string(out), "truncated") {
		t.Fatalf("missing truncation marker")
	}
}

func TestIsContextTooLongMessage(t *testing.T) {
	if !IsContextTooLongMessage(`This model's maximum prompt length is 500000 but the request contains 502555 tokens.`) {
		t.Fatal("should detect")
	}
}
