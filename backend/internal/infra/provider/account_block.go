package provider

import (
	"encoding/json"
	"strings"
)

// IsDefinitiveAccountBlockBody accepts only explicit error code or message signals.
// Used by Web chat/quota and gateway failure classification so blocked-user is not
// confused with temporary anti-bot / egress 403s.
func IsDefinitiveAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return IsDefinitiveAccountBlockText(string(body))
	}
	values := []string{
		jsonStringField(payload, "code"),
		jsonStringField(payload, "message"),
		jsonStringField(payload, "error"),
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		values = append(values,
			jsonStringField(nested, "code"),
			jsonStringField(nested, "message"),
			jsonStringField(nested, "error"),
		)
	}
	return IsDefinitiveAccountBlockText(strings.Join(values, " "))
}

// IsDefinitiveAccountBlockText reports blocked-user style permanent account bans.
func IsDefinitiveAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

func jsonStringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}
