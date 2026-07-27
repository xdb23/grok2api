package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	maxImportAccounts = 10000
	maxSSOTokenBytes  = provider.MaxSSOTokenBytes
)

type importDocument struct {
	Provider string        `json:"provider"`
	Accounts []importEntry `json:"accounts"`
}

type importEntry struct {
	Name              string     `json:"name"`
	Email             string     `json:"email,omitempty"`
	UserID            string     `json:"user_id,omitempty"`
	SSOToken          string     `json:"sso_token"`
	Token             string     `json:"token"`
	Tier              string     `json:"tier"`
	CloudflareCookies string     `json:"cloudflare_cookies"`
	NSFWEnabledAt     *time.Time `json:"nsfw_enabled_at,omitempty"`
	TOSAcceptedAt     *time.Time `json:"tos_accepted_at,omitempty"`
	TOSVersion        int        `json:"tos_version,omitempty"`
	BirthDateSetAt    *time.Time `json:"birth_date_set_at,omitempty"`
}

func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("账号文件中没有 Grok Web 账号")
	}
	// JSON document / NDJSON / top-level array must not fall through to plain-text
	// line import (that path previously treated `"name": "..."` lines as SSO tokens).
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return parseJSONCredentials(data)
	}
	return parsePlainTextCredentials(trimmed)
}

func parseJSONCredentials(data []byte) ([]provider.CredentialSeed, error) {
	entries, err := provider.DecodeCredentialJSONEntries[importEntry](data, string(account.ProviderWeb), maxImportAccounts)
	if err != nil {
		return nil, fmt.Errorf("解析 Grok Web 账号 JSON: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("账号文件中没有 Grok Web 账号")
	}
	seen := make(map[string]struct{}, len(entries))
	result := make([]provider.CredentialSeed, 0, len(entries))
	for index, entry := range entries {
		token, err := normalizeImportedSSOToken(firstNonEmpty(entry.SSOToken, entry.Token), index+1)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		tier := account.WebTier(strings.ToLower(strings.TrimSpace(entry.Tier)))
		if tier == "" {
			tier = account.WebTierAuto
		}
		if tier != account.WebTierAuto && tier != account.WebTierBasic && tier != account.WebTierSuper && tier != account.WebTierHeavy {
			return nil, fmt.Errorf("第 %d 个账号 tier 无效", index+1)
		}
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = fmt.Sprintf("Grok Web %s", security.HashToken(token)[:8])
		}
		result = append(result, provider.CredentialSeed{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: tier,
			Name: name, Email: strings.TrimSpace(entry.Email), UserID: strings.TrimSpace(entry.UserID),
			SourceKey: "sso:" + security.HashToken(token), AccessToken: token, CloudflareCookies: entry.CloudflareCookies,
			WebNSFWEnabledAt: entry.NSFWEnabledAt, WebTermsAcceptedAt: entry.TOSAcceptedAt,
			WebTermsAcceptedVersion: entry.TOSVersion, WebBirthDateSetAt: entry.BirthDateSetAt,
		})
	}
	return result, nil
}

func parsePlainTextCredentials(value string) ([]provider.CredentialSeed, error) {
	lines := strings.Split(value, "\n")
	seen := make(map[string]struct{}, len(lines))
	result := make([]provider.CredentialSeed, 0, len(lines))
	for index, line := range lines {
		raw := strings.TrimSpace(line)
		if raw == "" {
			continue
		}
		// Refuse JSON fragments that slipped into plain-text mode.
		if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "\"") {
			return nil, fmt.Errorf("第 %d 行疑似 JSON 片段，请使用 {\"provider\":\"grok_web\",\"accounts\":[...]} 或纯 JWT 行（eyJ...）", index+1)
		}
		token, err := normalizeImportedSSOToken(raw, index+1)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		result = append(result, provider.CredentialSeed{
			Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, WebTier: account.WebTierAuto,
			Name: "Grok Web " + security.HashToken(token)[:8], SourceKey: "sso:" + security.HashToken(token), AccessToken: token,
		})
		if len(result) > maxImportAccounts {
			return nil, provider.ErrCredentialLimit
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("文本中没有有效的 SSO JWT")
	}
	return result, nil
}

func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	document := importDocument{Provider: string(account.ProviderWeb), Accounts: make([]importEntry, 0, len(values))}
	for _, value := range values {
		document.Accounts = append(document.Accounts, importEntry{
			Name: value.Name, Email: value.Email, UserID: value.UserID, SSOToken: value.AccessToken,
			Tier: string(value.WebTier), CloudflareCookies: value.CloudflareCookies,
			NSFWEnabledAt: value.WebNSFWEnabledAt, TOSAcceptedAt: value.WebTermsAcceptedAt,
			TOSVersion: value.WebTermsAcceptedVersion, BirthDateSetAt: value.WebBirthDateSetAt,
		})
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func normalizeImportedSSOToken(raw string, index int) (string, error) {
	token := provider.SanitizeSSOToken(raw)
	if token == "" {
		return "", fmt.Errorf("第 %d 个账号缺少 sso_token", index)
	}
	if err := provider.ValidateSSOToken(token); err != nil {
		return "", fmt.Errorf("第 %d 个账号 SSO 无效: %w", index, err)
	}
	return token, nil
}

func sanitizeSSOToken(value string) string {
	return provider.SanitizeSSOToken(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
