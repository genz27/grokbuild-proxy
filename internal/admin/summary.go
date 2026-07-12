package admin

import (
	"strings"
	"time"

	"github.com/GreyGunG/grokbuild-proxy/internal/storage"
)

type poolSummary struct {
	Total     int `json:"total"`
	Enabled   int `json:"enabled"`
	Available int `json:"available"`
	Cooling   int `json:"cooling"`
	Disabled  int `json:"disabled"`
	// Expired counts enabled accounts whose access-token ExpiresAt is in the past.
	// This is NOT "account dead": with a refresh_token they remain selectable (Available).
	Expired int `json:"expired"`
	// NeedsRefresh is the subset of Expired that still has a refresh_token (auto-refreshable).
	NeedsRefresh int `json:"needs_refresh"`
	// Unrefreshable is Expired without refresh_token (true dead accounts).
	Unrefreshable  int        `json:"unrefreshable"`
	MissingTokens  int        `json:"missing_tokens"`
	NextRecoveryAt *time.Time `json:"next_recovery_at,omitempty"`
	LastSuccessAt  *time.Time `json:"last_success_at,omitempty"`
}

func summarizePool(creds []storage.Credential, now time.Time) poolSummary {
	summary := poolSummary{Total: len(creds)}
	for _, credential := range creds {
		if !credential.Enabled {
			summary.Disabled++
			continue
		}
		summary.Enabled++
		hasRefresh := strings.TrimSpace(credential.RefreshToken) != ""
		hasTokens := strings.TrimSpace(credential.AccessToken) != "" || hasRefresh
		if !hasTokens {
			summary.MissingTokens++
			continue
		}
		expired := !credential.ExpiresAt.IsZero() && !credential.ExpiresAt.After(now)
		if expired {
			summary.Expired++
			if hasRefresh {
				summary.NeedsRefresh++
			} else {
				summary.Unrefreshable++
			}
		}
		// Access token expired + no refresh → cannot recover; skip availability.
		if expired && !hasRefresh {
			continue
		}
		if credential.CooldownUntil != nil && credential.CooldownUntil.After(now) {
			summary.Cooling++
			if summary.NextRecoveryAt == nil || credential.CooldownUntil.Before(*summary.NextRecoveryAt) {
				value := credential.CooldownUntil.UTC()
				summary.NextRecoveryAt = &value
			}
		} else {
			summary.Available++
		}
		if credential.LastSuccessAt != nil &&
			(summary.LastSuccessAt == nil || credential.LastSuccessAt.After(*summary.LastSuccessAt)) {
			value := credential.LastSuccessAt.UTC()
			summary.LastSuccessAt = &value
		}
	}
	return summary
}
