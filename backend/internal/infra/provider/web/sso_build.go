package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	ssoBuildClientID = "b1a00492-073a-47ea-816f-4c329264a828"
	// Match the proven theyka/protocol_mint device OAuth scope (conversations + cli/api).
	// Soft accounts.x.ai warm-up + this scope is what succeeds for reserve SSO remints.
	ssoBuildScope = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write"
	// ssoBuildClientVersion is sent on Device OAuth CLI requests (device/code + token).
	// protocol_mint uses 0.2.93; keep a recent grok-shell for newer grants.
	ssoBuildClientVersion    = "0.2.111"
	ssoBuildClientIdentifier = "grok-shell"
	ssoBuildShellUA          = "grok-shell/" + ssoBuildClientVersion + " (linux; x86_64)"
	// Browser fingerprint for accounts.x.ai verify/approve (matches local mint BROWSER_UA).
	ssoBuildBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	ssoAccountsURL    = "https://accounts.x.ai/"
	// console session is used to resolve principal_id when Web account user_id is empty.
	ssoConsoleSessionURL = "https://console.x.ai/api/auth/session"
	ssoDeviceURL         = "https://auth.x.ai/oauth2/device/code"
	ssoVerifyURL         = "https://auth.x.ai/oauth2/device/verify"
	ssoApproveURL        = "https://auth.x.ai/oauth2/device/approve"
	ssoTokenURL          = "https://auth.x.ai/oauth2/token"
	maxAuthBody          = 2 << 20
)

// requestStyle selects CLI vs browser header profile for each OAuth step.
type requestStyle int

const (
	styleBrowser requestStyle = iota
	styleCLI
)

type ssoBuildHTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type ssoBuildFlow struct {
	client      ssoBuildHTTPClient
	userAgent   string
	cookies     map[string]string
	principalID string
	castleToken string
	lastReferer string
}

func (a *Adapter) ConvertToBuild(ctx context.Context, credential accountdomain.Credential) (provider.CredentialSeed, error) {
	if credential.Provider != accountdomain.ProviderWeb || credential.AuthType != accountdomain.AuthTypeSSO {
		return provider.CredentialSeed{}, fmt.Errorf("仅 Grok Web SSO 账号支持转换")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return provider.CredentialSeed{}, fmt.Errorf("解密 Grok Web SSO: %w", err)
	}
	token = normalizeSSOToken(token)
	if token == "" {
		return provider.CredentialSeed{}, provider.ErrUnauthorized
	}
	lease, err := a.egress.AcquireCredential(ctx, egressdomain.ScopeWeb, credential)
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	defer lease.Release()
	requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	flow := &ssoBuildFlow{
		client:      lease,
		userAgent:   lease.UserAgent,
		cookies:     map[string]string{"sso": token, "sso-rw": token},
		principalID: strings.TrimSpace(credential.UserID),
	}
	seed, err := flow.convert(requestCtx, credential)
	if err != nil {
		// Pure-Go Device Flow often gets accounts.x.ai HTTP 403 on approve for reserve SSO.
		// Fall back to the proven theyka mint_with_sso_protocol (Docker + curl_cffi + same proxy).
		if isConvertPortal403(err) && strings.TrimSpace(lease.ProxyURL) != "" {
			if mintSeed, mintErr := convertViaProtocolMint(requestCtx, token, lease.ProxyURL, credential.Email, credential.Name); mintErr == nil {
				a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
				return mintSeed, nil
			} else {
				err = fmt.Errorf("%w; protocol mint fallback: %v", err, mintErr)
			}
		}
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, conversionStatus(err), err)
		return provider.CredentialSeed{}, err
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, http.StatusOK, nil)
	return seed, nil
}

func (f *ssoBuildFlow) convert(ctx context.Context, credential accountdomain.Credential) (provider.CredentialSeed, error) {
	// 1) Skip hard accounts.x.ai portal warm-up.
	// protocol_mint may GET the portal, but a 403 there is non-fatal. In G2A, Lease.Do also
	// invalidates the browser/clearance session on any 403, which then poisons verify/approve.
	// SSO cookies are already attached on the flow; start Device OAuth directly.
	if strings.TrimSpace(f.principalID) == "" {
		f.principalID = strings.TrimSpace(credential.UserID)
	}

	// 2) Device code with CLI fingerprint. Include referrer=grok-build like protocol_mint
	// / oauth_device.request_device_code (form field; JWT may still omit referrer claim).
	form := url.Values{
		"client_id": {ssoBuildClientID},
		"scope":     {ssoBuildScope},
		"referrer":  {"grok-build"},
	}
	status, _, body, err := f.do(ctx, http.MethodPost, ssoDeviceURL, form, styleCLI)
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	if status < 200 || status >= 300 {
		return provider.CredentialSeed{}, fmt.Errorf("xAI Device Flow 启动失败: %w", conversionHTTPError{status: status})
	}
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &device); err != nil {
		return provider.CredentialSeed{}, fmt.Errorf("解析 xAI Device Flow: %w", err)
	}
	if device.DeviceCode == "" || device.UserCode == "" || !safeXAIURL(device.VerificationURIComplete) {
		return provider.CredentialSeed{}, fmt.Errorf("xAI Device Flow 返回字段不完整")
	}
	if device.Interval <= 0 {
		device.Interval = 5
	}
	if device.ExpiresIn <= 0 {
		device.ExpiresIn = 1800
	}

	// 4) Open verification page (browser). Some tenants land on consent directly.
	// protocol_mint does not hard-fail a non-2xx GET here; it always posts verify next.
	var finalURL string
	status, finalURL, body, err = f.do(ctx, http.MethodGet, device.VerificationURIComplete, nil, styleBrowser)
	if err != nil {
		status, finalURL, body = 0, device.VerificationURIComplete, nil
	}
	f.lastReferer = device.VerificationURIComplete
	if status >= 200 && status < 400 {
		f.harvestCastleFromHTML(string(body))
		f.harvestPrincipalFromHTML(string(body))
	}
	onConsent := strings.Contains(finalURL, "consent") || isConsentHTML(string(body))

	// 5) POST verify when GET did not already reach consent (mint always POSTs verify).
	if !onConsent {
		status, finalURL, body, err = f.do(ctx, http.MethodPost, ssoVerifyURL, url.Values{"user_code": {device.UserCode}}, styleBrowser)
		if err != nil {
			return provider.CredentialSeed{}, fmt.Errorf("SSO 自动验证 Device Flow 失败: %w", err)
		}
		// Accept 2xx/3xx, or a body/URL that already signals consent/done even on odd status.
		f.harvestCastleFromHTML(string(body))
		f.harvestPrincipalFromHTML(string(body))
		okConsent := strings.Contains(finalURL, "consent") || isConsentHTML(string(body)) || strings.Contains(finalURL, "done")
		if (status < 200 || status >= 400) && !okConsent {
			return provider.CredentialSeed{}, fmt.Errorf("SSO 自动验证 Device Flow 失败: %w", conversionHTTPError{status: status})
		}
		if !okConsent {
			return provider.CredentialSeed{}, fmt.Errorf("SSO 自动验证 Device Flow 失败")
		}
		if strings.Contains(finalURL, "consent") {
			f.lastReferer = finalURL
		} else {
			f.lastReferer = "https://accounts.x.ai/oauth2/device/consent?user_code=" + url.QueryEscape(device.UserCode)
		}
	} else if strings.Contains(finalURL, "consent") {
		f.lastReferer = finalURL
	} else {
		f.lastReferer = "https://accounts.x.ai/oauth2/device/consent?user_code=" + url.QueryEscape(device.UserCode)
	}

	// 6) Approve. Match protocol_mint fields exactly (empty principal_id is valid).
	// Do not attach harvested Castle tokens: mint never sends them, and bad tokens can 403.
	approve := url.Values{
		"user_code":      {device.UserCode},
		"action":         {"allow"},
		"principal_type": {"User"},
		"principal_id":   {f.principalID},
	}
	status, finalURL, _, err = f.do(ctx, http.MethodPost, ssoApproveURL, approve, styleBrowser)
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	if status < 200 || status >= 400 {
		return provider.CredentialSeed{}, fmt.Errorf("SSO 自动批准 Device Flow 失败: %w", conversionHTTPError{status: status})
	}
	if !strings.Contains(finalURL, "done") {
		return provider.CredentialSeed{}, fmt.Errorf("SSO 自动批准 Device Flow 失败")
	}

	// 7) Poll token with CLI fingerprint.
	token, err := f.pollToken(ctx, device.DeviceCode, time.Duration(device.Interval)*time.Second, time.Duration(device.ExpiresIn)*time.Second)
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	claims := decodeBuildClaims(firstValue(token.IDToken, token.AccessToken))
	userID := claimString(claims, "sub")
	email := claimString(claims, "email")
	teamID := claimString(claims, "team_id")
	name := strings.TrimSpace(credential.Name)
	if name == "" {
		name = "Grok Web account"
	}
	return provider.CredentialSeed{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: firstValue(email, name+" Build", userID, "Grok Build account"), Email: email, UserID: userID, TeamID: teamID,
		SourceKey: "sso-build:" + security.HashToken(token.AccessToken), OIDCClientID: ssoBuildClientID,
		AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: token.ExpiresAt,
	}, nil
}

type ssoBuildToken struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
}

func (f *ssoBuildFlow) pollToken(ctx context.Context, deviceCode string, interval, expiresIn time.Duration) (ssoBuildToken, error) {
	if interval < time.Second {
		interval = time.Second
	}
	deadline := time.Now().Add(min(expiresIn, 75*time.Second))
	for time.Now().Before(deadline) {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ssoBuildToken{}, ctx.Err()
		case <-timer.C:
		}
		status, _, body, err := f.do(ctx, http.MethodPost, ssoTokenURL, url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {ssoBuildClientID},
			"device_code": {deviceCode},
		}, styleCLI)
		if err != nil {
			return ssoBuildToken{}, err
		}
		var payload struct {
			AccessToken      string `json:"access_token"`
			RefreshToken     string `json:"refresh_token"`
			IDToken          string `json:"id_token"`
			ExpiresIn        int    `json:"expires_in"`
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return ssoBuildToken{}, fmt.Errorf("解析 xAI OAuth Token: %w", err)
		}
		if status >= 200 && status < 300 && payload.AccessToken != "" {
			if payload.ExpiresIn <= 0 {
				payload.ExpiresIn = 3600
			}
			return ssoBuildToken{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, IDToken: payload.IDToken, ExpiresAt: time.Now().UTC().Add(time.Duration(payload.ExpiresIn) * time.Second)}, nil
		}
		switch payload.Error {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied", "expired_token":
			return ssoBuildToken{}, provider.ErrAuthorizationDenied
		case "invalid_grant":
			return ssoBuildToken{}, fmt.Errorf("%w: %s", provider.ErrInvalidGrant, firstValue(payload.ErrorDescription, payload.Error))
		default:
			// Some gateways only put invalid_grant in the free-form description.
			combined := strings.ToLower(firstValue(payload.ErrorDescription, payload.Error))
			if strings.Contains(combined, "invalid_grant") {
				return ssoBuildToken{}, fmt.Errorf("%w: %s", provider.ErrInvalidGrant, firstValue(payload.ErrorDescription, payload.Error))
			}
			if status >= 400 {
				return ssoBuildToken{}, fmt.Errorf("xAI OAuth Token 失败 (%s): %w", firstValue(payload.ErrorDescription, payload.Error), conversionHTTPError{status: status})
			}
			return ssoBuildToken{}, fmt.Errorf("xAI OAuth Token 失败: %s", firstValue(payload.ErrorDescription, payload.Error, strconv.Itoa(status)))
		}
	}
	return ssoBuildToken{}, fmt.Errorf("xAI Device Flow 轮询超时")
}

func (f *ssoBuildFlow) resolvePrincipalID(ctx context.Context) string {
	status, _, body, err := f.do(ctx, http.MethodGet, ssoConsoleSessionURL, nil, styleBrowser)
	if err != nil || status < 200 || status >= 300 {
		return ""
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	// Shape: { "session": { "userId": "..." } } or flat userId.
	if session, ok := doc["session"].(map[string]any); ok {
		if uid := firstValue(asString(session["userId"]), asString(session["user_id"]), asString(session["sub"])); uid != "" {
			return uid
		}
	}
	return firstValue(asString(doc["userId"]), asString(doc["user_id"]), asString(doc["sub"]))
}

func (f *ssoBuildFlow) do(ctx context.Context, method, endpoint string, form url.Values, style requestStyle) (int, string, []byte, error) {
	if !safeXAIURL(endpoint) {
		return 0, "", nil, fmt.Errorf("xAI OAuth URL 不安全")
	}
	currentURL := endpoint
	currentMethod := method
	currentForm := form
	for redirects := 0; redirects <= 8; redirects++ {
		var body io.Reader
		if currentForm != nil {
			body = strings.NewReader(currentForm.Encode())
		}
		request, err := http.NewRequestWithContext(ctx, currentMethod, currentURL, body)
		if err != nil {
			return 0, "", nil, err
		}
		f.applyHeaders(request, style, currentForm != nil)
		response, err := f.client.Do(request)
		if err != nil {
			return 0, "", nil, err
		}
		f.captureCookies(response)
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxAuthBody+1))
		_ = response.Body.Close()
		if readErr != nil {
			return response.StatusCode, currentURL, nil, readErr
		}
		if len(data) > maxAuthBody {
			return response.StatusCode, currentURL, nil, fmt.Errorf("xAI OAuth 响应超过 2 MiB")
		}
		if response.StatusCode < 300 || response.StatusCode > 399 {
			return response.StatusCode, currentURL, data, nil
		}
		location := strings.TrimSpace(response.Header.Get("Location"))
		if location == "" {
			return response.StatusCode, currentURL, data, fmt.Errorf("xAI OAuth 重定向缺少 Location")
		}
		base, _ := url.Parse(currentURL)
		next, err := url.Parse(location)
		if err != nil {
			return response.StatusCode, currentURL, data, err
		}
		currentURL = base.ResolveReference(next).String()
		if !safeXAIURL(currentURL) {
			return response.StatusCode, currentURL, data, fmt.Errorf("xAI OAuth 重定向到非受信域名")
		}
		// Track browser referer for subsequent consent/approve posts.
		if style == styleBrowser {
			f.lastReferer = currentURL
		}
		if response.StatusCode == http.StatusSeeOther || ((response.StatusCode == http.StatusMovedPermanently || response.StatusCode == http.StatusFound) && currentMethod != http.MethodGet && currentMethod != http.MethodHead) {
			currentMethod = http.MethodGet
			currentForm = nil
		}
	}
	return 0, currentURL, nil, fmt.Errorf("xAI OAuth 重定向次数过多")
}

func (f *ssoBuildFlow) applyHeaders(request *http.Request, style requestStyle, hasForm bool) {
	request.Header.Set("Cookie", f.cookieHeader())
	switch style {
	case styleCLI:
		// grok-shell / CPA device_oauth request_session profile.
		request.Header.Set("Accept", "*/*")
		request.Header.Set("User-Agent", ssoBuildShellUA)
		request.Header.Set("x-grok-client-version", ssoBuildClientVersion)
		request.Header.Set("x-grok-client-identifier", ssoBuildClientIdentifier)
		request.Header.Set("x-grok-client-surface", "headless")
		request.Header.Set("x-grok-client-mode", "headless")
		request.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	default:
		// Browser profile for accounts.x.ai / console.x.ai.
		ua := firstValue(f.userAgent, ssoBuildBrowserUA)
		// Prefer a real Chrome UA when lease UA is empty or looks like a bot/shell.
		if strings.Contains(strings.ToLower(ua), "grok-shell") || strings.Contains(strings.ToLower(ua), "python") {
			ua = ssoBuildBrowserUA
		}
		request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json;q=0.8,*/*;q=0.7")
		request.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8")
		request.Header.Set("User-Agent", ua)
		request.Header.Set("sec-ch-ua", `"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"`)
		request.Header.Set("sec-ch-ua-mobile", "?0")
		request.Header.Set("sec-ch-ua-platform", `"Windows"`)
		// Also send client version so accounts side sees recent grok client.
		request.Header.Set("x-grok-client-version", ssoBuildClientVersion)
		request.Header.Set("x-grok-client-identifier", ssoBuildClientIdentifier)
		if host := request.URL.Host; strings.Contains(host, "x.ai") {
			if strings.Contains(host, "console.x.ai") {
				request.Header.Set("Origin", "https://console.x.ai")
				request.Header.Set("Referer", firstValue(f.lastReferer, "https://console.x.ai/"))
			} else {
				request.Header.Set("Origin", "https://accounts.x.ai")
				request.Header.Set("Referer", firstValue(f.lastReferer, "https://accounts.x.ai/"))
			}
			request.Header.Set("Sec-Fetch-Dest", "empty")
			request.Header.Set("Sec-Fetch-Mode", "cors")
			request.Header.Set("Sec-Fetch-Site", "same-site")
		}
		// Optional Castle request token as header (some frontends forward it this way).
		if token := strings.TrimSpace(f.castleToken); token != "" {
			request.Header.Set("x-castle-request-token", token)
			request.Header.Set("X-Castle-Request-Token", token)
		}
	}
	if hasForm {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
}

func (f *ssoBuildFlow) captureCookies(response *http.Response) {
	for _, cookie := range response.Cookies() {
		name := strings.TrimSpace(cookie.Name)
		value := strings.TrimSpace(cookie.Value)
		if name == "" || len(name) > 128 || len(value) > 16384 || strings.ContainsAny(name+value, "\r\n\x00") {
			continue
		}
		if cookie.MaxAge < 0 {
			delete(f.cookies, name)
			continue
		}
		f.cookies[name] = value
		// Castle / CF cookies sometimes carry a short-lived request token-like value.
		lower := strings.ToLower(name)
		if f.castleToken == "" && (strings.Contains(lower, "castle") || lower == "__cuid" || lower == "castle_id") && len(value) >= 20 {
			f.castleToken = value
		}
	}
}

func (f *ssoBuildFlow) cookieHeader() string {
	keys := make([]string, 0, len(f.cookies))
	for key := range f.cookies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+f.cookies[key])
	}
	return strings.Join(parts, "; ")
}

var (
	principalInputRe = regexp.MustCompile(`(?i)name=["']principal_id["'][^>]*value=["']([^"']*)["']|value=["']([^"']*)["'][^>]*name=["']principal_id["']`)
	castleTokenRe    = regexp.MustCompile(`(?i)(?:castleRequestToken|castle_request_token|createRequestToken)["'\s:=]+["']([A-Za-z0-9._\-]{20,})["']`)
	consentSignalRe  = regexp.MustCompile(`(?i)device/approve|principal_type|authorize grok build|action=["']allow["']`)
)

func (f *ssoBuildFlow) harvestPrincipalFromHTML(html string) {
	if f.principalID != "" || html == "" {
		return
	}
	if m := principalInputRe.FindStringSubmatch(html); len(m) > 0 {
		uid := strings.TrimSpace(firstValue(m[1], m[2]))
		if uid != "" {
			f.principalID = uid
		}
	}
}

func (f *ssoBuildFlow) harvestCastleFromHTML(html string) {
	if f.castleToken != "" || html == "" {
		return
	}
	if m := castleTokenRe.FindStringSubmatch(html); len(m) > 1 {
		if token := strings.TrimSpace(m[1]); len(token) >= 20 {
			f.castleToken = token
		}
	}
}

func isConsentHTML(html string) bool {
	return consentSignalRe.MatchString(html)
}

func safeXAIURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

func normalizeSSOToken(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sso=") {
		value = strings.TrimSpace(value[len("sso="):])
	}
	if token, _, found := strings.Cut(value, ";"); found {
		value = strings.TrimSpace(token)
	}
	return strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value)
}

func decodeBuildClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		return nil
	}
	return claims
}

func claimString(claims map[string]any, key string) string {
	value, _ := claims[key].(string)
	return strings.TrimSpace(value)
}

func asString(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strings.TrimSpace(strconv.FormatFloat(v, 'f', -1, 64))
	default:
		return ""
	}
}

func firstValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

type conversionHTTPError struct{ status int }

func (e conversionHTTPError) Error() string { return fmt.Sprintf("xAI OAuth HTTP %d", e.status) }

func conversionStatus(err error) int {
	var statusErr conversionHTTPError
	if errors.As(err, &statusErr) {
		return statusErr.status
	}
	if errors.Is(err, provider.ErrUnauthorized) {
		return http.StatusUnauthorized
	}
	return 0
}

var _ provider.BuildCredentialConverter = (*Adapter)(nil)
var _ ssoBuildHTTPClient = (*infraegress.Lease)(nil)
