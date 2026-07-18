package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestAutoCompactDropsOldMessages(t *testing.T) {
	msgs := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": "message " + strings.Repeat("x", 2000) + " " + string(rune('a'+(i%26))),
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":      "claude-sonnet-4",
		"messages":   msgs,
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
				"type":        "tool_result",
				"tool_use_id": "t1",
				"content":     big,
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
	if !IsContextTooLongMessage(`This model's maximum prompt length is 500000 but the request contains 1964042 tokens.`) {
		t.Fatal("should detect large overflow")
	}
}

func TestPrepareOpenAIBodyCompactsChat(t *testing.T) {
	msgs := make([]map[string]any, 0, 50)
	for i := 0; i < 50; i++ {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": "msg " + strings.Repeat("y", 3000) + fmt.Sprintf(" %d", i),
		})
		msgs = append(msgs, map[string]any{
			"role":    "tool",
			"content": strings.Repeat("tool-output-", 5000),
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":    "grok-4.5",
		"messages": msgs,
	})
	cfg := ContextGuardConfig{
		AutoCompact:        true,
		SoftInputTokens:    5000,
		MaxInputTokens:     20000,
		MaxToolResultChars: 800,
		KeepRecentMessages: 8,
	}
	out, res, err := PrepareOpenAIBody(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatalf("expected compact applied: %+v", res)
	}
	if res.EstimatedAfter >= res.EstimatedBefore {
		t.Fatalf("expected smaller after compact: before=%d after=%d", res.EstimatedBefore, res.EstimatedAfter)
	}
	if res.EstimatedAfter > cfg.MaxInputTokens {
		t.Fatalf("still over max: after=%d max=%d", res.EstimatedAfter, cfg.MaxInputTokens)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	arr, _ := parsed["messages"].([]any)
	if len(arr) == 0 || len(arr) > 20 {
		t.Fatalf("unexpected message count %d", len(arr))
	}
}

func TestPrepareOpenAIBodyCompactsResponsesInput(t *testing.T) {
	input := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		input = append(input, map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{{
				"type": "input_text",
				"text": strings.Repeat("z", 2500),
			}},
		})
		input = append(input, map[string]any{
			"type":   "function_call_output",
			"output": strings.Repeat("function-output-", 4000),
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model": "grok-4.5",
		"input": input,
	})
	cfg := ContextGuardConfig{
		AutoCompact:        true,
		SoftInputTokens:    4000,
		MaxInputTokens:     15000,
		MaxToolResultChars: 600,
		KeepRecentMessages: 6,
	}
	out, res, err := PrepareOpenAIBody(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.DroppedMessages <= 0 {
		t.Fatalf("expected input compact: %+v", res)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	arr, _ := parsed["input"].([]any)
	if len(arr) == 0 || len(arr) > 12 {
		t.Fatalf("unexpected input count %d", len(arr))
	}
}

func TestEmergencyCompactShrinksToolSchemasAndSystemPrompt(t *testing.T) {
	tools := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		tools = append(tools, map[string]any{
			"name":        fmt.Sprintf("tool_%d", i),
			"description": strings.Repeat("description ", 1000),
			"input_schema": map[string]any{
				"type":        "object",
				"description": strings.Repeat("schema ", 2000),
			},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":  "grok-4.5",
		"system": strings.Repeat("system instructions ", 10000),
		"tools":  tools,
		"messages": []map[string]any{{
			"role": "user", "content": "hello",
		}},
	})
	cfg := ContextGuardConfig{
		AutoCompact: true, SoftInputTokens: 10000, MaxInputTokens: 12000,
		MaxToolResultChars: 3000, KeepRecentMessages: 2,
	}
	out, result, err := PrepareAnthropicBody(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.EstimatedAfter > cfg.MaxInputTokens {
		t.Fatalf("emergency compact failed: %+v", result)
	}
	if len(out) >= len(body) {
		t.Fatalf("expected smaller body: before=%d after=%d", len(body), len(out))
	}
}

func TestAutoCompactKeepsTokenizerSafetyHeadroom(t *testing.T) {
	messages := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		messages = append(messages, map[string]any{
			"role": "user", "content": strings.Repeat("headroom ", 1000),
		})
	}
	body, _ := json.Marshal(map[string]any{"model": "grok-4.5", "messages": messages})
	cfg := ContextGuardConfig{
		AutoCompact: true, SoftInputTokens: 100000, MaxInputTokens: 100000,
		MaxToolResultChars: 3000, KeepRecentMessages: 8,
	}
	_, result, err := PrepareAnthropicBody(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result.EstimatedBefore >= cfg.SoftInputTokens {
		t.Fatalf("test body must be below configured soft limit: %+v", result)
	}
	if !result.Applied || result.EstimatedAfter > cfg.MaxInputTokens*4/5 {
		t.Fatalf("expected safety-headroom compaction: %+v", result)
	}
}
