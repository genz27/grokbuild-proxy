package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	openaisdk "github.com/openai/openai-go/v2"
	openaioption "github.com/openai/openai-go/v2/option"

	"github.com/GreyGunG/grokbuild-proxy/internal/anthropic"
	"github.com/GreyGunG/grokbuild-proxy/internal/config"
	"github.com/GreyGunG/grokbuild-proxy/internal/httpserver"
	"github.com/GreyGunG/grokbuild-proxy/internal/openai"
	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
)

func TestOfficialSDKContracts(t *testing.T) {
	const clientToken = "contract-client-token"
	store, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, _, _, _, err := store.EnsureBootstrapKeys(clientToken, "contract-admin-token"); err != nil {
		t.Fatal(err)
	}

	var captured struct {
		sync.Mutex
		body []byte
	}
	post := func(ctx context.Context, model, convID string, body []byte, stream bool) (*http.Response, error) {
		captured.Lock()
		captured.body = append(captured.body[:0], body...)
		captured.Unlock()
		if bytes.Contains(body, []byte(`"function_call_output"`)) &&
			bytes.Contains(body, []byte("cpa-thinking-tool")) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"id":"resp_cpa_final",
					"model":"grok-4.5",
					"status":"completed",
					"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"tool complete"}]}],
					"usage":{"input_tokens":8,"output_tokens":2}
				}`)),
			}, nil
		}
		if stream && bytes.Contains(body, []byte("cpa-thinking-stream")) {
			sse := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_cpa_stream","model":"grok-4.5"}}`,
				``,
				`data: {"type":"response.output_item.added","item":{"id":"rs_stream","type":"reasoning","encrypted_content":"enc_cpa_stream"}}`,
				``,
				`data: {"type":"response.reasoning_summary_part.added","item_id":"rs_stream"}`,
				``,
				`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_stream","delta":"Streaming thought."}`,
				``,
				`data: {"type":"response.reasoning_summary_part.done","item_id":"rs_stream"}`,
				``,
				`data: {"type":"response.output_item.done","item":{"id":"rs_stream","type":"reasoning","encrypted_content":"enc_cpa_stream"}}`,
				``,
				`data: {"type":"response.output_item.added","item":{"id":"fc_stream","call_id":"call_stream","type":"function_call","name":"inspect"}}`,
				``,
				`data: {"type":"response.function_call_arguments.done","item_id":"fc_stream","call_id":"call_stream","arguments":"{}"}`,
				``,
				`data: {"type":"response.completed","response":{"id":"resp_cpa_stream","status":"completed","usage":{"input_tokens":4,"output_tokens":3}}}`,
				``,
			}, "\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		}
		if bytes.Contains(body, []byte("cpa-thinking-tool")) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{
					"id":"resp_cpa_tool",
					"model":"grok-4.5",
					"status":"completed",
					"output":[
						{"type":"reasoning","id":"rs_cpa","summary":[{"type":"summary_text","text":"I need the tool."}],"encrypted_content":"enc_cpa_roundtrip"},
						{"type":"function_call","id":"fc_cpa","call_id":"call_cpa","name":"inspect","arguments":"{}"}
					],
					"usage":{"input_tokens":4,"output_tokens":3}
				}`)),
			}, nil
		}
		if stream {
			sse := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_sdk_stream","model":"grok-4.5"}}`,
				``,
				`data: {"type":"response.output_text.delta","item_id":"msg_contract","delta":"hello from stream"}`,
				``,
				`data: {"type":"response.completed","response":{"id":"resp_sdk_stream","status":"completed","usage":{"input_tokens":2,"output_tokens":3},"output":[{"id":"msg_contract","type":"message","content":[{"type":"output_text","text":"hello from stream"}]}]}}`,
				``,
			}, "\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"id":"resp_sdk_json",
				"model":"grok-4.5",
				"status":"completed",
				"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from json"}]}],
				"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}
			}`)),
		}, nil
	}

	cfg := config.Default()
	handler := httpserver.New(httpserver.Options{
		Config: cfg,
		Store:  store,
		OpenAI: &openai.Handlers{Post: post},
		Anthropic: &anthropic.Handlers{
			Cfg:          cfg.Anthropic,
			Post:         post,
			ResolveModel: cfg.ResolveModel,
		},
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Run("OpenAI chat non-stream", func(t *testing.T) {
		client := openaisdk.NewClient(
			openaioption.WithBaseURL(server.URL+"/v1"),
			openaioption.WithAPIKey(clientToken),
		)
		completion, err := client.Chat.Completions.New(context.Background(), openaisdk.ChatCompletionNewParams{
			Model: "grok-4.5",
			Messages: []openaisdk.ChatCompletionMessageParamUnion{
				{
					OfUser: &openaisdk.ChatCompletionUserMessageParam{
						Content: openaisdk.ChatCompletionUserMessageParamContentUnion{
							OfString: openaisdk.String("hello"),
						},
					},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "hello from json" {
			t.Fatalf("completion=%+v", completion)
		}
	})

	t.Run("OpenAI chat stream", func(t *testing.T) {
		client := openaisdk.NewClient(
			openaioption.WithBaseURL(server.URL+"/v1"),
			openaioption.WithAPIKey(clientToken),
		)
		stream := client.Chat.Completions.NewStreaming(context.Background(), openaisdk.ChatCompletionNewParams{
			Model: "grok-4.5",
			Messages: []openaisdk.ChatCompletionMessageParamUnion{
				{
					OfUser: &openaisdk.ChatCompletionUserMessageParam{
						Content: openaisdk.ChatCompletionUserMessageParamContentUnion{
							OfString: openaisdk.String("hello"),
						},
					},
				},
			},
		})
		defer stream.Close()
		var text strings.Builder
		for stream.Next() {
			chunk := stream.Current()
			for _, choice := range chunk.Choices {
				text.WriteString(choice.Delta.Content)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		if text.String() != "hello from stream" {
			t.Fatalf("stream text=%q", text.String())
		}
	})

	t.Run("Anthropic Messages non-stream", func(t *testing.T) {
		client := anthropicsdk.NewClient(
			anthropicoption.WithBaseURL(server.URL),
			anthropicoption.WithAPIKey(clientToken),
		)
		message, err := client.Messages.New(context.Background(), anthropicsdk.MessageNewParams{
			Model:     "claude-sonnet-4",
			MaxTokens: 64,
			Messages:  []anthropicsdk.MessageParam{anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock("hello"))},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(message.Content) != 1 || message.Content[0].Text != "hello from json" {
			t.Fatalf("message=%+v", message)
		}
		if message.StopReason != anthropicsdk.StopReasonEndTurn {
			t.Fatalf("stop_reason=%q", message.StopReason)
		}
	})

	t.Run("Anthropic adaptive thinking effort", func(t *testing.T) {
		client := anthropicsdk.NewClient(
			anthropicoption.WithBaseURL(server.URL),
			anthropicoption.WithAPIKey(clientToken),
		)
		message, err := client.Messages.New(context.Background(), anthropicsdk.MessageNewParams{
			Model:     "claude-opus-4-6",
			MaxTokens: 16_000,
			Messages:  []anthropicsdk.MessageParam{anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock("solve this"))},
			Thinking: anthropicsdk.ThinkingConfigParamUnion{
				OfAdaptive: &anthropicsdk.ThinkingConfigAdaptiveParam{
					Display: anthropicsdk.ThinkingConfigAdaptiveDisplayOmitted,
				},
			},
			OutputConfig: anthropicsdk.OutputConfigParam{
				Effort: anthropicsdk.OutputConfigEffortMax,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(message.Content) != 1 || message.Content[0].Text != "hello from json" {
			t.Fatalf("message=%+v", message)
		}

		captured.Lock()
		body := append([]byte(nil), captured.body...)
		captured.Unlock()
		var upstreamBody map[string]any
		if err := json.Unmarshal(body, &upstreamBody); err != nil {
			t.Fatalf("upstream body: %v: %s", err, body)
		}
		if upstreamBody["model"] != "grok-4.5" {
			t.Fatalf("upstream model=%v body=%s", upstreamBody["model"], body)
		}
		reasoning, _ := upstreamBody["reasoning"].(map[string]any)
		if reasoning["effort"] != "high" {
			t.Fatalf("reasoning=%v body=%s", reasoning, body)
		}
		if _, ok := upstreamBody["thinking"]; ok {
			t.Fatalf("native thinking leaked upstream: %s", body)
		}
		if _, ok := upstreamBody["output_config"]; ok {
			t.Fatalf("native output_config leaked upstream: %s", body)
		}
	})

	t.Run("Anthropic CPA thinking tool round trip", func(t *testing.T) {
		client := anthropicsdk.NewClient(
			anthropicoption.WithBaseURL(server.URL),
			anthropicoption.WithAPIKey(clientToken),
		)
		userMessage := anthropicsdk.NewUserMessage(
			anthropicsdk.NewTextBlock("cpa-thinking-tool"),
		)
		thinking := anthropicsdk.ThinkingConfigParamUnion{
			OfAdaptive: &anthropicsdk.ThinkingConfigAdaptiveParam{
				Display: anthropicsdk.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
		outputConfig := anthropicsdk.OutputConfigParam{
			Effort: anthropicsdk.OutputConfigEffortHigh,
		}
		tools := []anthropicsdk.ToolUnionParam{{
			OfTool: &anthropicsdk.ToolParam{
				Name:        "inspect",
				Description: anthropicsdk.String("Inspect the requested resource"),
				InputSchema: anthropicsdk.ToolInputSchemaParam{
					Properties: map[string]any{},
				},
			},
		}}
		first, err := client.Messages.New(context.Background(), anthropicsdk.MessageNewParams{
			Model:        "claude-opus-4-6",
			MaxTokens:    16_000,
			Messages:     []anthropicsdk.MessageParam{userMessage},
			Thinking:     thinking,
			OutputConfig: outputConfig,
			Tools:        tools,
		})
		if err != nil {
			t.Fatal(err)
		}
		if first.StopReason != anthropicsdk.StopReasonToolUse {
			t.Fatalf("first stop_reason=%q content=%+v", first.StopReason, first.Content)
		}
		firstJSON, err := json.Marshal(first)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(firstJSON, []byte(`"type":"thinking"`)) ||
			!bytes.Contains(firstJSON, []byte(`"signature":"enc_cpa_roundtrip"`)) {
			t.Fatalf("missing CPA thinking block: %s", firstJSON)
		}

		toolUseID := "call_cpa"
		for _, block := range first.Content {
			if block.Type == "tool_use" && block.ID != "" {
				toolUseID = block.ID
				break
			}
		}
		toolResult := anthropicsdk.NewUserMessage(
			anthropicsdk.NewToolResultBlock(toolUseID, "inspection result", false),
		)
		second, err := client.Messages.New(context.Background(), anthropicsdk.MessageNewParams{
			Model:        "claude-opus-4-6",
			MaxTokens:    16_000,
			Messages:     []anthropicsdk.MessageParam{userMessage, first.ToParam(), toolResult},
			Thinking:     thinking,
			OutputConfig: outputConfig,
			Tools:        tools,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Content) != 1 || second.Content[0].Text != "tool complete" {
			t.Fatalf("second=%+v", second)
		}

		captured.Lock()
		secondBody := append([]byte(nil), captured.body...)
		captured.Unlock()
		var upstreamBody struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal(secondBody, &upstreamBody); err != nil {
			t.Fatalf("second upstream body: %v: %s", err, secondBody)
		}
		var replayIndex, callIndex, outputIndex = -1, -1, -1
		for i, item := range upstreamBody.Input {
			switch item["type"] {
			case "reasoning":
				if item["encrypted_content"] == "enc_cpa_roundtrip" {
					replayIndex = i
				}
			case "function_call":
				if item["call_id"] == toolUseID {
					callIndex = i
				}
			case "function_call_output":
				if item["call_id"] == toolUseID {
					outputIndex = i
				}
			}
		}
		if replayIndex < 0 || callIndex < 0 || outputIndex < 0 ||
			!(replayIndex < callIndex && callIndex < outputIndex) {
			t.Fatalf(
				"CPA replay order reasoning=%d call=%d output=%d body=%s",
				replayIndex,
				callIndex,
				outputIndex,
				secondBody,
			)
		}
	})

	t.Run("Anthropic CPA thinking stream", func(t *testing.T) {
		client := anthropicsdk.NewClient(
			anthropicoption.WithBaseURL(server.URL),
			anthropicoption.WithAPIKey(clientToken),
		)
		stream := client.Messages.NewStreaming(context.Background(), anthropicsdk.MessageNewParams{
			Model:     "claude-opus-4-6",
			MaxTokens: 16_000,
			Messages: []anthropicsdk.MessageParam{
				anthropicsdk.NewUserMessage(
					anthropicsdk.NewTextBlock("cpa-thinking-stream"),
				),
			},
			Thinking: anthropicsdk.ThinkingConfigParamUnion{
				OfAdaptive: &anthropicsdk.ThinkingConfigAdaptiveParam{
					Display: anthropicsdk.ThinkingConfigAdaptiveDisplaySummarized,
				},
			},
			OutputConfig: anthropicsdk.OutputConfigParam{
				Effort: anthropicsdk.OutputConfigEffortHigh,
			},
			Tools: []anthropicsdk.ToolUnionParam{{
				OfTool: &anthropicsdk.ToolParam{
					Name:        "inspect",
					Description: anthropicsdk.String("Inspect the requested resource"),
					InputSchema: anthropicsdk.ToolInputSchemaParam{
						Properties: map[string]any{},
					},
				},
			}},
		})
		defer stream.Close()
		var message anthropicsdk.Message
		for stream.Next() {
			if err := message.Accumulate(stream.Current()); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		if message.StopReason != anthropicsdk.StopReasonToolUse {
			t.Fatalf("stop_reason=%q content=%+v", message.StopReason, message.Content)
		}
		messageJSON, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(messageJSON, []byte(`"thinking":"Streaming thought."`)) ||
			!bytes.Contains(messageJSON, []byte(`"signature":"enc_cpa_stream"`)) ||
			!bytes.Contains(messageJSON, []byte(`"type":"tool_use"`)) {
			t.Fatalf("streamed CPA message=%s", messageJSON)
		}
	})

	t.Run("Anthropic Messages stream", func(t *testing.T) {
		client := anthropicsdk.NewClient(
			anthropicoption.WithBaseURL(server.URL),
			anthropicoption.WithAPIKey(clientToken),
		)
		stream := client.Messages.NewStreaming(context.Background(), anthropicsdk.MessageNewParams{
			Model:     "claude-sonnet-4",
			MaxTokens: 64,
			Messages:  []anthropicsdk.MessageParam{anthropicsdk.NewUserMessage(anthropicsdk.NewTextBlock("hello"))},
		})
		defer stream.Close()
		var message anthropicsdk.Message
		for stream.Next() {
			if err := message.Accumulate(stream.Current()); err != nil {
				t.Fatal(err)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		if len(message.Content) != 1 || message.Content[0].Text != "hello from stream" {
			t.Fatalf("message=%+v", message)
		}
		if message.StopReason != anthropicsdk.StopReasonEndTurn {
			t.Fatalf("stop_reason=%q", message.StopReason)
		}
	})
}
