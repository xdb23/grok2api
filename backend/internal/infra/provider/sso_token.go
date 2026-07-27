package provider

import (
	"fmt"
	"strings"
	"unicode"
)

const (
	// MaxSSOTokenBytes is the hard cap for a single SSO cookie value.
	MaxSSOTokenBytes = 16 << 10
	// minSSOJWTBytes rejects truncated / placeholder tokens that are too short to be session JWTs.
	minSSOJWTBytes = 40
)

// SanitizeSSOToken normalizes common paste formats (sso=..., cookie lists) into a bare token.
func SanitizeSSOToken(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sso=") {
		value = strings.TrimSpace(value[len("sso="):])
	}
	if token, _, found := strings.Cut(value, ";"); found {
		value = strings.TrimSpace(token)
	}
	return strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value)
}

// ValidateSSOToken enforces the import/convert rule: Grok Web/Console SSO must be a JWT cookie
// (typically starts with eyJ…, three base64url segments). This blocks the common failure mode
// where pretty-printed JSON lines such as `"name": "…"` or `"sso_token": "eyJ…` were imported
// as plain-text tokens and later cloned into Console accounts that 401 immediately.
func ValidateSSOToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("SSO token 为空")
	}
	if len(token) > MaxSSOTokenBytes {
		return fmt.Errorf("SSO token 超过 %d 字节", MaxSSOTokenBytes)
	}
	if len(token) < minSSOJWTBytes {
		return fmt.Errorf("SSO token 过短，不是有效的 JWT 会话")
	}
	// Reject obvious JSON fragments from array/pretty-print paste mistakes.
	if strings.ContainsAny(token, "\"{}[]:, \t") {
		return fmt.Errorf("SSO token 含非法字符（疑似把 JSON 行当成 token 导入）")
	}
	if !strings.HasPrefix(token, "eyJ") {
		return fmt.Errorf("SSO token 必须以 JWT 前缀 eyJ 开头")
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 || len(parts) > 3 {
		// xAI session cookies are standard 3-part JWTs; allow 2-part unsigned for rare cases.
		return fmt.Errorf("SSO token 不是合法 JWT 结构（需要 2–3 段 base64url）")
	}
	for _, part := range parts {
		if part == "" || !isBase64URLToken(part) {
			return fmt.Errorf("SSO token JWT 段包含非法字符")
		}
	}
	return nil
}

// LooksLikeSSOJWT reports whether token already passes ValidateSSOToken.
func LooksLikeSSOJWT(token string) bool {
	return ValidateSSOToken(token) == nil
}

func isBase64URLToken(value string) bool {
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '=' {
			continue
		}
		return false
	}
	return true
}
