package anthropic

import (
	"sort"
	"strings"

	"github.com/GreyGunG/grokbuild-proxy/internal/config"
	"github.com/GreyGunG/grokbuild-proxy/internal/upstream"
)

// ClaudeCode1MContextWindow is the context size Claude Code assigns to
// model ids ending in "[1m]". Advertise this on alias rows so long-session
// pickers and client-side budget math line up with the 1M option.
const ClaudeCode1MContextWindow = 1_000_000

// ModelEntry is an OpenAI-list-shaped model row used by GET /v1/models.
type ModelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object,omitempty"`
	Created       int64  `json:"created,omitempty"`
	OwnedBy       string `json:"owned_by,omitempty"`
	Name          string `json:"name,omitempty"`
	Description   string `json:"description,omitempty"`
	ContextWindow int    `json:"context_window,omitempty"`
	APIBackend    string `json:"api_backend,omitempty"`
	UpstreamModel string `json:"-"`
	AliasOf       string `json:"-"`
}

// AliasModels generates Claude-facing alias entries from upstream models + config aliases.
// Claude Code discovery only accepts ids that start with "claude" or "anthropic".
//
// For each base Claude alias we also emit a sibling with the "[1m]" suffix.
// Claude Code treats that suffix as a local 1M context marker; the proxy strips
// it in ResolveModel before upstream mapping.
func AliasModels(upstreamModels []upstream.Model, cfg config.AnthropicConfig) []ModelEntry {
	byID := make(map[string]upstream.Model, len(upstreamModels))
	for _, m := range upstreamModels {
		if strings.TrimSpace(m.ID) != "" {
			byID[m.ID] = m
		}
	}

	keys := make([]string, 0, len(cfg.ModelAliases))
	for k := range cfg.ModelAliases {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]ModelEntry, 0, len(keys)*2)
	seen := map[string]struct{}{}
	for _, alias := range keys {
		target := strings.TrimSpace(cfg.ModelAliases[alias])
		if target == "" {
			continue
		}
		if !isClaudeFacingID(alias) {
			// short names like "sonnet" are env defaults, not discovery ids
			continue
		}
		// Skip operator-provided [1m] keys; we synthesize them from the base id.
		if strings.HasSuffix(strings.ToLower(alias), "[1m]") {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}

		base := ModelEntry{
			ID:            alias,
			Object:        "model",
			OwnedBy:       "anthropic",
			UpstreamModel: target,
			AliasOf:       target,
			APIBackend:    "responses",
			Name:          alias,
		}
		if um, ok := byID[target]; ok {
			base.Created = um.Created
			if um.Name != "" {
				base.Name = um.Name
			}
			base.Description = um.Description
			base.ContextWindow = um.ContextWindow
			if um.APIBackend != "" {
				base.APIBackend = um.APIBackend
			}
		}
		if base.ContextWindow <= 0 {
			// Default Claude-facing window when upstream catalog is empty.
			base.ContextWindow = 200_000
		}
		out = append(out, base)

		// 1M sibling for Claude Code long-session picker / budget.
		oneM := base
		oneM.ID = alias + "[1m]"
		if oneM.Name != "" {
			oneM.Name = oneM.Name + " (1M context)"
		} else {
			oneM.Name = oneM.ID
		}
		if oneM.Description == "" {
			oneM.Description = "1M context window variant for long Claude Code sessions"
		} else {
			oneM.Description = oneM.Description + " (1M context)"
		}
		oneM.ContextWindow = ClaudeCode1MContextWindow
		if _, ok := seen[oneM.ID]; !ok {
			seen[oneM.ID] = struct{}{}
			out = append(out, oneM)
		}
	}
	return out
}

func isClaudeFacingID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, "claude") || strings.HasPrefix(id, "anthropic")
}
