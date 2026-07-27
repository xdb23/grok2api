package console

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

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
	Name              string `json:"name"`
	Email             string `json:"email,omitempty"`
	UserID            string `json:"user_id,omitempty"`
	SSOToken          string `json:"sso_token"`
	Token             string `json:"token"`
	CloudflareCookies string `json:"cloudflare_cookies"`
}

func parseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, fmt.Errorf("账号文件中没有 Grok Console 账号")
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return parseJSONCredentials(data)
	}
	return parsePlainTextCredentials(trimmed)
}

func parseJSONCredentials(data []byte) ([]provider.CredentialSeed, error) {
	entries, err := provider.DecodeCredentialJSONEntries[importEntry](data, string(account.ProviderConsole), maxImportAccounts)
	if err != nil {
		return nil, fmt.Errorf("解析 Grok Console 账号 JSON: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("账号文件中没有 Grok Console 账号")
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
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = "Grok Console " + security.HashToken(token)[:8]
		}
		seed := credentialSeed(name, token)
		seed.Email = strings.TrimSpace(entry.Email)
		seed.UserID = strings.TrimSpace(entry.UserID)
		seed.CloudflareCookies = entry.CloudflareCookies
		result = append(result, seed)
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
		if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "\"") {
			return nil, fmt.Errorf("第 %d 行疑似 JSON 片段，请使用 {\"provider\":\"grok_console\",\"accounts\":[...]} 或纯 JWT 行（eyJ...）", index+1)
		}
		token, err := normalizeImportedSSOToken(raw, index+1)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[token]; exists {
			continue
		}
		seen[token] = struct{}{}
		result = append(result, credentialSeed("Grok Console "+security.HashToken(token)[:8], token))
		if len(result) > maxImportAccounts {
			return nil, provider.ErrCredentialLimit
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("文本中没有有效的 SSO JWT")
	}
	return result, nil
}

func credentialSeed(name, token string) provider.CredentialSeed {
	return provider.CredentialSeed{
		Provider: account.ProviderConsole, AuthType: account.AuthTypeSSO, Name: name,
		SourceKey: "console-sso:" + security.HashToken(token), AccessToken: token,
	}
}

func marshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	document := importDocument{Provider: string(account.ProviderConsole), Accounts: make([]importEntry, 0, len(values))}
	for _, value := range values {
		document.Accounts = append(document.Accounts, importEntry{Name: value.Name, Email: value.Email, UserID: value.UserID, SSOToken: value.AccessToken, CloudflareCookies: value.CloudflareCookies})
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
