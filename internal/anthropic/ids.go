package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// newMessageID returns an Anthropic-style message id: msg_<hex>.
func newMessageID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("msg_%d", time.Now().UnixNano())
	}
	return "msg_" + hex.EncodeToString(b[:])
}

// normalizeMessageID forces Anthropic message id shape.
// Upstream Grok returns UUID / resp_* ids which fail msg_ prefix checks.
func normalizeMessageID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return newMessageID()
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	id = strings.TrimPrefix(id, "resp_")
	id = strings.TrimPrefix(id, "response_")
	// Strip non-alnum except underscore/dash, then map to msg_.
	cleaned := make([]rune, 0, len(id))
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			cleaned = append(cleaned, r)
		}
	}
	s := strings.ReplaceAll(string(cleaned), "-", "")
	if s == "" {
		return newMessageID()
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return "msg_" + strings.ToLower(s)
}

// normalizeToolUseID forces Anthropic tool_use id shape toolu_*.
// Upstream Grok returns call-* / call_* / fc_* ids.
func normalizeToolUseID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return newToolUseID()
	}
	if strings.HasPrefix(id, "toolu_") {
		return id
	}
	for _, p := range []string{"call-", "call_", "fc_", "function_call_", "tool_", "tool-"} {
		if strings.HasPrefix(id, p) {
			id = strings.TrimPrefix(id, p)
			break
		}
	}
	cleaned := make([]rune, 0, len(id))
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			cleaned = append(cleaned, r)
		}
	}
	s := strings.ReplaceAll(string(cleaned), "-", "")
	if s == "" {
		return newToolUseID()
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return "toolu_" + strings.ToLower(s)
}

func newToolUseID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("toolu_%d", time.Now().UnixNano())
	}
	return "toolu_" + hex.EncodeToString(b[:])
}
