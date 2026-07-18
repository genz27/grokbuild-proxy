package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// StreamState tracks Claude SSE emission while reading Responses SSE.
type StreamState struct {
	MsgID        string
	Model        string
	RequestModel string
	HasToolUse   bool
	Started      bool // message_start emitted
	Finished     bool
	Failed       bool
	Usage        Usage
}

type streamBlock struct {
	Index   int
	Kind    string
	CallID  string
	Name    string
	Emitted bool
	Stopped bool
}

// StreamTranslator converts Responses SSE events into Anthropic SSE events.
// It never buffers the full upstream response; callers feed chunks via ProcessLine
// or use PipeResponsesSSE for a full stream copy.
type StreamTranslator struct {
	State StreamState
	// Out is where Claude SSE frames are written (includes "event:" / "data:" lines).
	Out io.Writer
	// Flusher optional HTTP flusher.
	Flusher           http.Flusher
	Thinking          ThinkingBridgeOptions
	ContextManagement map[string]any

	blocks              map[string]*streamBlock
	nextIndex           int
	thinkingBlock       *streamBlock
	thinkingSignature   string
	thinkingStopPending bool
	currentReasoningKey string
	completedReasoning  map[string]struct{}
}

// NewStreamTranslator builds a translator that writes Claude SSE to w.
func NewStreamTranslator(
	w io.Writer,
	flusher http.Flusher,
	requestModel string,
	thinking ...ThinkingBridgeOptions,
) *StreamTranslator {
	translator := &StreamTranslator{
		Out:     w,
		Flusher: flusher,
		State: StreamState{
			RequestModel: requestModel,
			Model:        requestModel,
		},
		blocks:             make(map[string]*streamBlock),
		completedReasoning: make(map[string]struct{}),
	}
	if len(thinking) > 0 {
		translator.Thinking = thinking[0]
	}
	return translator
}

func (t *StreamTranslator) flush() {
	if t.Flusher != nil {
		t.Flusher.Flush()
	}
}

func (t *StreamTranslator) writeEvent(event string, payload any) error {
	var data []byte
	switch v := payload.(type) {
	case []byte:
		data = v
	case string:
		data = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		data = b
	}
	var buf bytes.Buffer
	if event != "" {
		buf.WriteString("event: ")
		buf.WriteString(event)
		buf.WriteByte('\n')
	}
	buf.WriteString("data: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	if _, err := t.Out.Write(buf.Bytes()); err != nil {
		return err
	}
	t.flush()
	return nil
}

// EnsureStart emits message_start if not yet emitted.
func (t *StreamTranslator) EnsureStart(id, model string) error {
	if t.State.Started {
		return nil
	}
	id = normalizeMessageID(id)
	if model == "" {
		model = t.State.RequestModel
	}
	if model == "" {
		model = t.State.Model
	}
	t.State.MsgID = id
	t.State.Model = model
	t.State.Started = true
	payload := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  t.State.Usage.InputTokens,
				"output_tokens": 0,
			},
		},
	}
	return t.writeEvent("message_start", payload)
}

func (t *StreamTranslator) ensureTextBlock(key string) (*streamBlock, error) {
	if err := t.finishThinkingBeforeContent(); err != nil {
		return nil, err
	}
	key = normalizeBlockKey("text", key)
	if block := t.blocks[key]; block != nil {
		return block, nil
	}
	block := &streamBlock{Index: t.nextIndex, Kind: "text"}
	t.nextIndex++
	payload := map[string]any{
		"type":  "content_block_start",
		"index": block.Index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}
	if err := t.writeEvent("content_block_start", payload); err != nil {
		return nil, err
	}
	t.blocks[key] = block
	return block, nil
}

func (t *StreamTranslator) ensureToolBlock(key, callID, name string) (*streamBlock, error) {
	if err := t.finishThinkingBeforeContent(); err != nil {
		return nil, err
	}
	key = normalizeBlockKey("tool", firstNonEmptyStream(key, callID))
	if block := t.blocks[key]; block != nil {
		return block, nil
	}
	if callID == "" {
		callID = strings.TrimPrefix(key, "tool:")
	}
	callID = normalizeToolUseID(callID)
	if name == "" {
		name = "tool"
	}
	block := &streamBlock{
		Index:  t.nextIndex,
		Kind:   "tool",
		CallID: callID,
		Name:   name,
	}
	t.nextIndex++
	payload := map[string]any{
		"type":  "content_block_start",
		"index": block.Index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    callID,
			"name":  name,
			"input": map[string]any{},
		},
	}
	if err := t.writeEvent("content_block_start", payload); err != nil {
		return nil, err
	}
	t.blocks[key] = block
	t.State.HasToolUse = true
	return block, nil
}

func (t *StreamTranslator) stopBlock(key, kind string) error {
	key = normalizeBlockKey(kind, key)
	block := t.blocks[key]
	if block == nil || block.Stopped {
		return nil
	}
	if err := t.writeEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": block.Index,
	}); err != nil {
		return err
	}
	block.Stopped = true
	return nil
}

func (t *StreamTranslator) stopAllBlocks() error {
	blocks := make([]*streamBlock, 0, len(t.blocks))
	for _, block := range t.blocks {
		if !block.Stopped {
			blocks = append(blocks, block)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Index < blocks[j].Index })
	for _, block := range blocks {
		if err := t.writeEvent("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": block.Index,
		}); err != nil {
			return err
		}
		block.Stopped = true
	}
	return nil
}

func (t *StreamTranslator) textDelta(key, text string) error {
	if text == "" {
		return nil
	}
	block, err := t.ensureTextBlock(key)
	if err != nil {
		return err
	}
	if block.Stopped {
		return nil
	}
	payload := map[string]any{
		"type":  "content_block_delta",
		"index": block.Index,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	}
	if err := t.writeEvent("content_block_delta", payload); err != nil {
		return err
	}
	block.Emitted = true
	return nil
}

func (t *StreamTranslator) toolArgsDelta(key, callID, name, args string) error {
	if args == "" {
		return nil
	}
	block, err := t.ensureToolBlock(key, callID, name)
	if err != nil {
		return err
	}
	if block.Stopped {
		return nil
	}
	payload := map[string]any{
		"type":  "content_block_delta",
		"index": block.Index,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": args,
		},
	}
	if err := t.writeEvent("content_block_delta", payload); err != nil {
		return err
	}
	block.Emitted = true
	return nil
}

func normalizeBlockKey(kind, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "default"
	}
	prefix := kind + ":"
	if strings.HasPrefix(key, prefix) {
		return key
	}
	return prefix + key
}

// Finish emits message_delta + message_stop.
func (t *StreamTranslator) Finish(stopReason string, usage Usage) error {
	if t.State.Finished {
		return nil
	}
	if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
		return err
	}
	if err := t.finalizeThinkingBlock(true); err != nil {
		return err
	}
	if err := t.stopAllBlocks(); err != nil {
		return err
	}
	if stopReason == "" {
		if t.State.HasToolUse {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	if usage.InputTokens == 0 && t.State.Usage.InputTokens > 0 {
		usage.InputTokens = t.State.Usage.InputTokens
	}
	if usage.OutputTokens == 0 && t.State.Usage.OutputTokens > 0 {
		usage.OutputTokens = t.State.Usage.OutputTokens
	}
	delta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":  usage.InputTokens,
			"output_tokens": usage.OutputTokens,
		},
	}
	if t.ContextManagement != nil {
		delta["context_management"] = t.ContextManagement
	}
	if err := t.writeEvent("message_delta", delta); err != nil {
		return err
	}
	if err := t.writeEvent("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	t.State.Finished = true
	return nil
}

// Fail emits a terminal Anthropic stream error. It never emits message_stop.
func (t *StreamTranslator) Fail(status int, message string) error {
	if t.State.Finished {
		return nil
	}
	if err := t.finalizeThinkingBlock(true); err != nil {
		return err
	}
	if err := t.stopAllBlocks(); err != nil {
		return err
	}
	if err := t.writeEvent("error", NewErrorEnvelope(status, message)); err != nil {
		return err
	}
	t.State.Failed = true
	t.State.Finished = true
	return nil
}

// ProcessData handles one JSON payload from upstream SSE (without "data:" prefix).
func (t *StreamTranslator) ProcessData(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		if t.State.Started && !t.State.Finished {
			return t.Finish("", t.State.Usage)
		}
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("anthropic stream: invalid JSON event: %w", err)
	}
	typ := rawString(root["type"])

	switch typ {
	case "response.created", "response.in_progress":
		id := ""
		model := t.State.RequestModel
		if r, ok := root["response"]; ok {
			var resp struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage Usage  `json:"usage"`
			}
			_ = json.Unmarshal(r, &resp)
			id = resp.ID
			if resp.Model != "" && model == "" {
				model = resp.Model
			}
			if resp.Usage.InputTokens > 0 {
				t.State.Usage.InputTokens = resp.Usage.InputTokens
			}
		}
		if t.State.RequestModel != "" {
			model = t.State.RequestModel
		}
		return t.EnsureStart(id, model)

	case "response.output_text.delta":
		if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
			return err
		}
		return t.textDelta(streamEventKey(root), rawString(root["delta"]))

	case "response.reasoning_summary_part.added":
		if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
			return err
		}
		return t.startThinkingPart(reasoningEventKey(root))

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
			return err
		}
		if t.thinkingBlock == nil {
			if err := t.startThinkingPart(reasoningEventKey(root)); err != nil {
				return err
			}
		}
		return t.thinkingDelta(rawString(root["delta"]))

	case "response.reasoning_summary_part.done":
		t.thinkingStopPending = true
		return nil

	case "response.content_part.added":
		var part struct {
			Part struct {
				Type string `json:"type"`
			} `json:"part"`
		}
		_ = json.Unmarshal(data, &part)
		if part.Part.Type == "output_text" {
			if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
				return err
			}
			_, err := t.ensureTextBlock(streamEventKey(root))
			return err
		}
		return nil

	case "response.content_part.done":
		var part struct {
			Part struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"part"`
		}
		_ = json.Unmarshal(data, &part)
		if part.Part.Type == "output_text" {
			key := streamEventKey(root)
			block := t.blocks[normalizeBlockKey("text", key)]
			if (block == nil || !block.Emitted) && part.Part.Text != "" {
				if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
					return err
				}
				if err := t.textDelta(key, part.Part.Text); err != nil {
					return err
				}
			}
			return t.stopBlock(key, "text")
		}
		return nil

	case "response.output_item.added":
		if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
			return err
		}
		var wrap struct {
			Item struct {
				Type             string `json:"type"`
				ID               string `json:"id"`
				CallID           string `json:"call_id"`
				Name             string `json:"name"`
				Arguments        string `json:"arguments"`
				EncryptedContent string `json:"encrypted_content"`
			} `json:"item"`
		}
		_ = json.Unmarshal(data, &wrap)
		if wrap.Item.Type == "reasoning" {
			t.currentReasoningKey = normalizeReasoningKey(firstNonEmptyStream(
				wrap.Item.ID,
				reasoningEventKey(root),
			))
			t.captureThinkingSignature(t.currentReasoningKey, wrap.Item.EncryptedContent)
			return nil
		}
		if wrap.Item.Type == "function_call" {
			callID := wrap.Item.CallID
			if callID == "" {
				callID = wrap.Item.ID
			}
			key := streamItemKey(wrap.Item.ID, callID, streamEventKey(root))
			if _, err := t.ensureToolBlock(key, callID, wrap.Item.Name); err != nil {
				return err
			}
			if wrap.Item.Arguments != "" {
				return t.toolArgsDelta(key, callID, wrap.Item.Name, wrap.Item.Arguments)
			}
		}
		return nil

	case "response.function_call_arguments.delta":
		if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
			return err
		}
		callID := rawString(root["call_id"])
		if callID == "" {
			callID = rawString(root["item_id"])
		}
		key := streamItemKey(rawString(root["item_id"]), callID, streamEventKey(root))
		return t.toolArgsDelta(key, callID, rawString(root["name"]), rawString(root["delta"]))

	case "response.function_call_arguments.done":
		if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
			return err
		}
		callID := rawString(root["call_id"])
		if callID == "" {
			callID = rawString(root["item_id"])
		}
		key := streamItemKey(rawString(root["item_id"]), callID, streamEventKey(root))
		block, err := t.ensureToolBlock(key, callID, rawString(root["name"]))
		if err != nil {
			return err
		}
		if !block.Emitted {
			if args := rawString(root["arguments"]); args != "" {
				if err := t.toolArgsDelta(key, callID, rawString(root["name"]), args); err != nil {
					return err
				}
			}
		}
		return t.stopBlock(key, "tool")

	case "response.output_item.done":
		var wrap struct {
			Item struct {
				Type             string          `json:"type"`
				ID               string          `json:"id"`
				CallID           string          `json:"call_id"`
				Name             string          `json:"name"`
				Arguments        string          `json:"arguments"`
				Content          json.RawMessage `json:"content"`
				Summary          json.RawMessage `json:"summary"`
				EncryptedContent string          `json:"encrypted_content"`
			} `json:"item"`
		}
		_ = json.Unmarshal(data, &wrap)
		if wrap.Item.Type == "reasoning" {
			key := normalizeReasoningKey(firstNonEmptyStream(
				wrap.Item.ID,
				reasoningEventKey(root),
			))
			t.currentReasoningKey = key
			t.captureThinkingSignature(key, wrap.Item.EncryptedContent)
			if t.thinkingBlock == nil {
				item := map[string]json.RawMessage{
					"id":                json.RawMessage(strconv.Quote(wrap.Item.ID)),
					"summary":           wrap.Item.Summary,
					"encrypted_content": json.RawMessage(strconv.Quote(wrap.Item.EncryptedContent)),
				}
				return t.emitFinalReasoningItem(item, key)
			}
			return t.finalizeThinkingBlock(true)
		}
		if wrap.Item.Type == "function_call" {
			if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
				return err
			}
			callID := wrap.Item.CallID
			if callID == "" {
				callID = wrap.Item.ID
			}
			key := streamItemKey(wrap.Item.ID, callID, streamEventKey(root))
			block, err := t.ensureToolBlock(key, callID, wrap.Item.Name)
			if err != nil {
				return err
			}
			if !block.Emitted && wrap.Item.Arguments != "" {
				if err := t.toolArgsDelta(key, callID, wrap.Item.Name, wrap.Item.Arguments); err != nil {
					return err
				}
			}
			return t.stopBlock(key, "tool")
		}
		if wrap.Item.Type == "message" {
			key := streamItemKey(wrap.Item.ID, streamEventKey(root))
			block := t.blocks[normalizeBlockKey("text", key)]
			if block != nil && block.Emitted {
				return t.stopBlock(key, "text")
			}
			for _, bl := range extractMessageTextBlocks(wrap.Item.Content) {
				if bl.Type == "text" && bl.Text != "" {
					if err := t.EnsureStart(t.State.MsgID, t.State.Model); err != nil {
						return err
					}
					if err := t.textDelta(key, bl.Text); err != nil {
						return err
					}
				}
			}
			return t.stopBlock(key, "text")
		}
		return nil

	case "response.completed", "response.incomplete":
		stop := "end_turn"
		usage := t.State.Usage
		var wrap struct {
			Response struct {
				ID     string          `json:"id"`
				Model  string          `json:"model"`
				Status string          `json:"status"`
				Usage  json.RawMessage `json:"usage"`
				Output json.RawMessage `json:"output"`
			} `json:"response"`
		}
		_ = json.Unmarshal(data, &wrap)
		if wrap.Response.ID != "" && t.State.MsgID == "" {
			_ = t.EnsureStart(wrap.Response.ID, t.State.RequestModel)
		} else {
			_ = t.EnsureStart(t.State.MsgID, t.State.Model)
		}
		if len(wrap.Response.Usage) > 0 {
			usage = extractUsage(wrap.Response.Usage)
			t.State.Usage = usage
		}
		if len(wrap.Response.Output) > 0 {
			if err := t.emitFinalOutput(wrap.Response.Output); err != nil {
				return err
			}
		}
		if t.State.HasToolUse {
			stop = "tool_use"
		} else if typ == "response.incomplete" {
			stop = "max_tokens"
		}
		return t.Finish(stop, usage)

	case "response.failed":
		status, msg := streamFailure(root, data)
		return t.Fail(status, msg)

	case "error":
		msg := FormatErrorMessage(500, data)
		return t.Fail(http.StatusBadGateway, msg)

	default:
		return nil
	}
}

func (t *StreamTranslator) emitFinalOutput(raw json.RawMessage) error {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	for i, item := range items {
		switch rawString(item["type"]) {
		case "reasoning":
			if err := t.emitFinalReasoningItem(
				item,
				fmt.Sprintf("final-reasoning-%d", i),
			); err != nil {
				return err
			}
		case "message":
			key := streamItemKey(rawString(item["id"]), fmt.Sprintf("final-%d", i))
			block := t.blocks[normalizeBlockKey("text", key)]
			if block == nil && t.hasEmittedKind("text") {
				continue
			}
			if block != nil && block.Emitted {
				if err := t.stopBlock(key, "text"); err != nil {
					return err
				}
				continue
			}
			for _, text := range extractMessageTextBlocks(item["content"]) {
				if text.Type == "text" && text.Text != "" {
					if err := t.textDelta(key, text.Text); err != nil {
						return err
					}
				}
			}
			if err := t.stopBlock(key, "text"); err != nil {
				return err
			}
		case "function_call":
			callID := firstNonEmptyStream(rawString(item["call_id"]), rawString(item["id"]))
			key := streamItemKey(rawString(item["id"]), callID, fmt.Sprintf("final-%d", i))
			block, err := t.ensureToolBlock(key, callID, rawString(item["name"]))
			if err != nil {
				return err
			}
			if !block.Emitted {
				args := rawString(item["arguments"])
				if args == "" {
					args = "{}"
				}
				if err := t.toolArgsDelta(key, callID, rawString(item["name"]), args); err != nil {
					return err
				}
			}
			if err := t.stopBlock(key, "tool"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *StreamTranslator) hasEmittedKind(kind string) bool {
	for _, block := range t.blocks {
		if block.Kind == kind && block.Emitted {
			return true
		}
	}
	return false
}

func streamEventKey(root map[string]json.RawMessage) string {
	return streamItemKey(
		rawString(root["item_id"]),
		rawString(root["output_index"]),
		rawString(root["content_index"]),
	)
}

func streamItemKey(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "default"
}

func firstNonEmptyStream(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func streamFailure(root map[string]json.RawMessage, raw []byte) (int, string) {
	status := http.StatusBadGateway
	if code := rawString(root["status_code"]); code != "" {
		if parsed, err := strconv.Atoi(code); err == nil && parsed >= 400 && parsed <= 599 {
			status = parsed
		}
	}
	var wrap struct {
		Response struct {
			Error struct {
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(raw, &wrap)
	message := strings.TrimSpace(wrap.Response.Error.Message)
	if message == "" {
		message = strings.TrimSpace(wrap.Response.Error.Code)
	}
	if message == "" {
		message = "upstream response failed"
	}
	return status, message
}

// ProcessLine handles one raw SSE line (may be "data: ...", "event: ...", or empty).
func (t *StreamTranslator) ProcessLine(line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		return t.ProcessData(payload)
	}
	if len(line) > 0 && line[0] == '{' {
		return t.ProcessData(line)
	}
	return nil
}

// PipeResponsesSSE reads upstream SSE from r and writes Claude SSE to the translator.
// It does not buffer the full response.
func PipeResponsesSSE(r io.Reader, t *StreamTranslator) error {
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	// Large tool-call SSE lines (Claude Code file rewrites) can exceed 4MiB.
	sc.Buffer(buf, 32*1024*1024)
	var dataBuf []byte
	flush := func() error {
		if len(dataBuf) == 0 {
			return nil
		}
		payload := bytes.TrimSuffix(dataBuf, []byte("\n"))
		dataBuf = nil
		return t.ProcessData(payload)
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(dataBuf) > 0 {
				dataBuf = append(dataBuf, '\n')
			}
			dataBuf = append(dataBuf, payload...)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if t.State.Started && !t.State.Finished {
		return io.ErrUnexpectedEOF
	}
	if !t.State.Started && !t.State.Finished {
		return io.ErrUnexpectedEOF
	}
	return nil
}

// WriteClaudeSSEHeaders sets SSE headers for Anthropic streaming.
func WriteClaudeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}
