package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// UpstreamFailure 保存可安全暴露给下游和审计的上游失败分类，不包含响应正文或凭据。
type UpstreamFailure struct {
	HTTPStatus             int
	Code                   string
	PublicMessage          string
	UpstreamCode           string
	AccountID              uint64
	AccountName            string
	AccountScoped          bool
	AccountBlocked         bool
	PermanentAccountDenial bool
	QuotaExhausted         bool
	FreeQuotaExhausted     bool
	ModelQuotaExhausted    bool
	CredentialRejected     bool
	// PlatformBusy is a transient upstream capacity signal (not free-usage).
	// Do not cool the account; may still rotate to another account.
	PlatformBusy bool
	// RequestScoped failures are about this prompt/content (e.g. safety policy).
	// Do not cool the account, refresh OAuth, or rotate accounts.
	RequestScoped bool
	Fingerprint   string
	RetryAfter    time.Duration
	Cause         error
}

func (e *UpstreamFailure) Error() string {
	if e == nil {
		return "上游请求失败"
	}
	if e.UpstreamCode != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.UpstreamCode)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Cause)
	}
	return e.Code
}

func (e *UpstreamFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *UpstreamFailure) AuditCode() string {
	if e == nil {
		return "upstream_error"
	}
	if suffix := normalizeFailureCode(e.UpstreamCode); suffix != "" {
		return truncateFailureCode(e.Code + "_" + suffix)
	}
	return truncateFailureCode(e.Code)
}

// ClientCredentialErrorCode 返回允许暴露给客户端的账号类上游错误码。
// HTTP 状态和错误文案仍由传输层统一脱敏；这里只放行稳定、无凭据内容的机器码。
func (e *UpstreamFailure) ClientCredentialErrorCode() string {
	if e == nil {
		return "upstream_unavailable"
	}
	return clientCredentialErrorCode(e.HTTPStatus, e.UpstreamCode)
}

// ClientCredentialErrorCodeFromBody 从账号类上游错误正文中提取允许公开的机器码。
// 用于上游响应已直接交给传输层、尚未构造 UpstreamFailure 的路径。
func ClientCredentialErrorCodeFromBody(status int, body []byte) string {
	upstreamCode, _, _ := extractUpstreamErrorMetadata(body)
	return clientCredentialErrorCode(status, upstreamCode)
}

func clientCredentialErrorCode(status int, upstreamCode string) string {
	if status == http.StatusForbidden && normalizeFailureCode(upstreamCode) == "permission_denied" {
		return "permission-denied"
	}
	return "upstream_unavailable"
}

func newHTTPUpstreamFailure(status int, body []byte, accountID uint64, accountName string) *UpstreamFailure {
	upstreamCode, upstreamType, upstreamMessage := extractUpstreamErrorMetadata(body)
	failure := &UpstreamFailure{
		HTTPStatus: status, Code: "upstream_error", PublicMessage: "上游服务返回错误",
		UpstreamCode: upstreamCode, AccountID: accountID, AccountName: accountName,
	}
	if status < 400 || status > 599 {
		failure.HTTPStatus = http.StatusBadGateway
	}
	metadataText := strings.ToLower(strings.Join([]string{upstreamCode, upstreamType, upstreamMessage}, " "))
	switch status {
	case http.StatusUnauthorized:
		failure.Code = "upstream_unauthorized"
		failure.PublicMessage = "上游账号认证失败"
		failure.AccountScoped = true
		failure.CredentialRejected = true
		failure.AccountBlocked = isDefinitiveAccountBlock(metadataText)
	case http.StatusPaymentRequired:
		failure.Code = "upstream_payment_required"
		failure.PublicMessage = "上游账号额度不足"
		failure.AccountScoped = true
		failure.QuotaExhausted = true
		// spending-limit is account-scoped, but its paid/free recovery kind depends on
		// the selected account's billing snapshot and must be decided by the selector.
		failure.FreeQuotaExhausted = isFreeQuotaExhaustion(metadataText)
	case http.StatusForbidden:
		failure.Code = "upstream_forbidden"
		failure.PublicMessage = "上游拒绝了该请求"
		// Safety denials are request-scoped: inspect structured metadata and raw body so
		// SAFETY_CHECK_TYPE_* markers still match when nested only in free text.
		if isContentSafetyRejection(metadataText, upstreamCode) || isContentSafetyRejection(strings.ToLower(string(body)), upstreamCode) {
			// Request-level safety / policy: fail this prompt only (upstream #781 + XDB PlatformBusy split).
			failure.RequestScoped = true
			failure.AccountScoped = false
			failure.Code = "upstream_content_policy"
			failure.PublicMessage = "上游认为内容不符合使用规范"
			break
		}
		failure.AccountBlocked = isDefinitiveAccountBlock(metadataText) || provider.IsDefinitiveAccountBlockBody(body)
		// Permanent denial uses the human message, not bare machine codes like permission-denied
		// that are shared by request-level policy denials (upstream classification fix).
		failure.PermanentAccountDenial = isPermanentAccountDenial(upstreamMessage)
		failure.ModelQuotaExhausted = isModelQuotaExhaustion(metadataText)
		failure.FreeQuotaExhausted = failure.ModelQuotaExhausted || isFreeQuotaExhaustion(metadataText)
		failure.QuotaExhausted = failure.FreeQuotaExhausted || isPaidQuotaExhaustion(metadataText)
		failure.CredentialRejected = !failure.QuotaExhausted && containsAny(metadataText, "authentication", "unauthorized", "invalid token", "token expired")
		failure.AccountScoped = failure.AccountBlocked || failure.PermanentAccountDenial || failure.QuotaExhausted || failure.CredentialRejected || isAccountScopedForbidden(metadataText)
	case http.StatusTooManyRequests:
		failure.Code = "upstream_rate_limited"
		failure.PublicMessage = "上游请求频率受限"
		failure.ModelQuotaExhausted = isModelQuotaExhaustion(metadataText)
		failure.FreeQuotaExhausted = failure.ModelQuotaExhausted || isFreeQuotaExhaustion(metadataText)
		failure.QuotaExhausted = failure.FreeQuotaExhausted || isPaidQuotaExhaustion(metadataText)
		// Platform capacity (qg 0.3.25+): do not treat as free-usage or cool the account.
		if !failure.QuotaExhausted && isPlatformCapacityBusy(metadataText, upstreamCode) {
			failure.PlatformBusy = true
			failure.AccountScoped = false
			failure.Code = "upstream_capacity"
			failure.PublicMessage = "上游当前繁忙，请稍后重试"
			break
		}
		failure.AccountScoped = true
	default:
		failure.Code = "upstream_server_error"
		failure.PublicMessage = "上游服务暂时异常"
	}
	fingerprintPart := normalizeFailureCode(firstNonEmptyFailure(upstreamCode, upstreamType, upstreamMessage))
	if fingerprintPart == "" {
		fingerprintPart = "unknown"
	}
	failure.Fingerprint = fmt.Sprintf("%d:%s", status, fingerprintPart)
	return failure
}

func newTransportUpstreamFailure(err error, accountID uint64, accountName string) *UpstreamFailure {
	code, message := "upstream_network_error", "连接上游服务失败"
	status := http.StatusBadGateway
	if neterrorpkg.IsResponseHeaderTimeout(err) {
		status, code, message = http.StatusGatewayTimeout, "upstream_header_timeout", "等待上游响应头超时"
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, message = "upstream_timeout", "上游服务响应超时"
	}
	return &UpstreamFailure{
		HTTPStatus: status, Code: code, PublicMessage: message,
		AccountID: accountID, AccountName: accountName, Fingerprint: code, Cause: err,
	}
}

func newCredentialUpstreamFailure(err error, accountID uint64, accountName string) *UpstreamFailure {
	return &UpstreamFailure{
		HTTPStatus: http.StatusBadGateway, Code: "upstream_credential_unavailable", PublicMessage: "上游账号凭据不可用",
		AccountID: accountID, AccountName: accountName, AccountScoped: true, Cause: err,
	}
}

func extractUpstreamErrorMetadata(body []byte) (string, string, string) {
	if len(body) == 0 {
		return "", "", ""
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return "", "", strings.TrimSpace(string(body))
	}
	root, ok := payload.(map[string]any)
	if !ok {
		return "", "", ""
	}
	if nested, ok := root["error"].(map[string]any); ok {
		code := firstNonEmptyFailure(firstStringValue(nested, "code", "error_code"), firstStringValue(root, "code", "error_code"))
		errorType := firstNonEmptyFailure(firstStringValue(nested, "type", "error_type"), firstStringValue(root, "type", "error_type"))
		message := firstNonEmptyFailure(firstStringValue(nested, "message", "error"), firstStringValue(root, "message"))
		return code, errorType, message
	}
	message := firstNonEmptyFailure(firstStringValue(root, "error"), firstStringValue(root, "message"))
	return firstStringValue(root, "code", "error_code"), firstStringValue(root, "type", "error_type"), message
}

func isAccountScopedForbidden(text string) bool {
	// Do not match bare "permission" / permission-denied alone: those codes are shared by
	// request-level policy denials. Account scope requires quota/billing/auth wording.
	return containsAny(text, "quota", "billing", "subscription", "entitlement", "unauthorized", "authentication", "invalid token", "token expired", "usage-exhausted", "insufficient", "spending-limit")
}

func isPermanentAccountDenial(text string) bool {
	text = strings.ToLower(strings.Trim(strings.TrimSpace(text), " .!\t\r\n"))
	return strings.Contains(text, "access to the chat endpoint is denied") || text == "access denied"
}

func isDefinitiveAccountBlock(text string) bool {
	return provider.IsDefinitiveAccountBlockText(text)
}

func isPaidQuotaExhaustion(text string) bool {
	return strings.Contains(text, "personal-team-blocked:spending-limit")
}

func isFreeQuotaExhaustion(text string) bool {
	return containsAny(text, "subscription:free-usage-exhausted", "used all the included free usage for model")
}

func isModelQuotaExhaustion(text string) bool {
	return strings.Contains(text, "used all the included free usage for model")
}

// isPlatformCapacityBusy mirrors cpa-xai-quota-guard 0.3.25: overloaded / high demand
// must not cool free-usage accounts. Explicit free-usage signals win over capacity.
func isPlatformCapacityBusy(text, upstreamCode string) bool {
	if isFreeQuotaExhaustion(text) {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(upstreamCode))
	if code == "resource-exhausted" || strings.Contains(code, "resource_exhausted") {
		return true
	}
	return containsAny(text,
		"resource-exhausted", "resource_exhausted",
		"at capacity", "high demand", "overloaded", "over load", "temporarily unavailable",
		"server is busy", "try again later", "capacity",
	)
}

// isContentSafetyRejection identifies prompt/content policy failures that must not
// rotate accounts or invalidate OAuth (see upstream draft #781).
func isContentSafetyRejection(text, upstreamCode string) bool {
	code := strings.ToLower(strings.TrimSpace(upstreamCode))
	if strings.Contains(code, "safety_check") || strings.HasPrefix(code, "safety_") {
		return true
	}
	return containsAny(text,
		"safety_check_type", "safety-check", "content violates usage guidelines",
		"violates usage guidelines", "content policy", "usage guidelines",
		"disallowed content", "unsafe content",
	)
}

func containsAny(text string, signals ...string) bool {
	for _, signal := range signals {
		if strings.Contains(text, signal) {
			return true
		}
	}
	return false
}

func firstStringValue(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyFailure(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func normalizeFailureCode(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var builder strings.Builder
	for _, current := range value {
		switch {
		case unicode.IsLetter(current), unicode.IsDigit(current):
			builder.WriteRune(current)
		case current == '-', current == '_', current == '.', current == ':':
			builder.WriteByte('_')
		}
		if builder.Len() >= 48 {
			break
		}
	}
	return strings.Trim(builder.String(), "_")
}

func truncateFailureCode(value string) string {
	if len(value) <= 100 {
		return value
	}
	return value[:100]
}
