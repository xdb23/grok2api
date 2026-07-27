package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

type scriptedSSOClient struct {
	responses []*http.Response
	requests  []*http.Request
}

func (c *scriptedSSOClient) Do(request *http.Request) (*http.Response, error) {
	c.requests = append(c.requests, request)
	response := c.responses[0]
	c.responses = c.responses[1:]
	return response, nil
}

func TestSSOBuildFlowFollowsOnlyTrustedXAIHTTPSRedirects(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://auth.x.ai/next"}, "Set-Cookie": []string{"session=abc; Path=/; Secure"}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))},
	}}
	flow := &ssoBuildFlow{client: client, userAgent: "test-agent", cookies: map[string]string{"sso": "secret"}}
	status, finalURL, body, err := flow.do(context.Background(), http.MethodGet, ssoAccountsURL, nil, styleBrowser)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || finalURL != "https://auth.x.ai/next" || string(body) != "ok" {
		t.Fatalf("response = %d %s %q", status, finalURL, body)
	}
	if len(client.requests) != 2 || client.requests[1].Header.Get("User-Agent") != "test-agent" {
		t.Fatalf("requests = %#v", client.requests)
	}
	cookie := client.requests[1].Header.Get("Cookie")
	if !strings.Contains(cookie, "sso=secret") || !strings.Contains(cookie, "session=abc") {
		t.Fatalf("redirect cookies = %q", cookie)
	}
	// Browser style should include Chrome client hints and version headers.
	if client.requests[0].Header.Get("x-grok-client-version") != ssoBuildClientVersion {
		t.Fatalf("browser version header = %q", client.requests[0].Header.Get("x-grok-client-version"))
	}

	unsafe := &scriptedSSOClient{responses: []*http.Response{{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://example.com/steal"}}, Body: io.NopCloser(strings.NewReader(""))}}}
	flow = &ssoBuildFlow{client: unsafe, userAgent: "test-agent", cookies: map[string]string{"sso": "secret"}}
	if _, _, _, err := flow.do(context.Background(), http.MethodGet, ssoAccountsURL, nil, styleBrowser); err == nil {
		t.Fatal("unsafe redirect was accepted")
	}
}

func TestSSOBuildCLIStyleUsesShellUA(t *testing.T) {
	client := &scriptedSSOClient{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"device_code":"d"}`))},
	}}
	flow := &ssoBuildFlow{client: client, cookies: map[string]string{"sso": "secret"}}
	if _, _, _, err := flow.do(context.Background(), http.MethodPost, ssoDeviceURL, nil, styleCLI); err != nil {
		t.Fatal(err)
	}
	req := client.requests[0]
	if req.Header.Get("User-Agent") != ssoBuildShellUA {
		t.Fatalf("cli ua = %q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("x-grok-client-version") != ssoBuildClientVersion || req.Header.Get("x-grok-client-surface") != "headless" {
		t.Fatalf("cli headers = %#v", req.Header)
	}
}

func TestSSOBuildHarvestPrincipalAndCastle(t *testing.T) {
	flow := &ssoBuildFlow{}
	flow.harvestPrincipalFromHTML(`<input name="principal_id" value="user-123"/>`)
	if flow.principalID != "user-123" {
		t.Fatalf("principal = %q", flow.principalID)
	}
	flow.harvestCastleFromHTML(`window.__CASTLE__ = { castleRequestToken: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.castle" }`)
	if !strings.HasPrefix(flow.castleToken, "eyJ") {
		t.Fatalf("castle = %q", flow.castleToken)
	}
	if !isConsentHTML(`<form action="/oauth2/device/approve"><input name="principal_type"/>`) {
		t.Fatal("expected consent html")
	}
}

func TestSSOBuildConversionSanitizesTokenAndURLs(t *testing.T) {
	if token := normalizeSSOToken("sso=token-value; x-userid=drop"); token != "token-value" {
		t.Fatalf("token = %q", token)
	}
	for _, value := range []string{"https://accounts.x.ai/", "https://auth.x.ai/oauth2/device/code"} {
		if !safeXAIURL(value) {
			t.Fatalf("trusted URL rejected: %s", value)
		}
	}
	for _, value := range []string{"http://auth.x.ai/", "https://x.ai.example.com/", "https://user@auth.x.ai/"} {
		if safeXAIURL(value) {
			t.Fatalf("unsafe URL accepted: %s", value)
		}
	}
}

func TestSSOBuildConvertMintCompatibleDeviceFormAndFlow(t *testing.T) {
	// Convert starts at device code (no portal warm-up) and uses mint-compatible form fields.
	accessParts := []string{
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","email":"a@b.c","team_id":"t1"}`)),
		"sig",
	}
	accessToken := strings.Join(accessParts, ".")
	tokenJSON := `{"access_token":"` + accessToken + `","refresh_token":"refresh-1","id_token":"` + accessToken + `","expires_in":3600}`
	client := &scriptedSSOClient{responses: []*http.Response{
		// 1) device code
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"device_code":"dev","user_code":"USER","verification_uri_complete":"https://accounts.x.ai/oauth2/device?user_code=USER","interval":1,"expires_in":600}`))},
		// 2) open verification → already consent HTML
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`<html>authorize grok build<form action="/oauth2/device/approve">`))},
		// 3-4) approve redirects to done
		{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://accounts.x.ai/oauth2/device/done"}}, Body: io.NopCloser(strings.NewReader(""))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("done"))},
		// 5) poll token after interval sleep
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tokenJSON))},
	}}

	flow := &ssoBuildFlow{client: client, cookies: map[string]string{"sso": "secret"}, principalID: "user-1"}
	seed, err := flow.convert(context.Background(), accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", UserID: "user-1",
	})
	if err != nil {
		t.Fatalf("convert failed: %v", err)
	}
	if seed.AccessToken != accessToken || seed.RefreshToken != "refresh-1" || seed.UserID != "user-1" {
		t.Fatalf("seed = %#v", seed)
	}
	var deviceReq *http.Request
	for _, req := range client.requests {
		if strings.Contains(req.URL.Path, "/oauth2/device/code") {
			deviceReq = req
			break
		}
	}
	if deviceReq == nil {
		t.Fatal("device code request missing")
	}
	body, _ := io.ReadAll(deviceReq.Body)
	form := string(body)
	if !strings.Contains(form, "referrer=grok-build") || !strings.Contains(form, "conversations") {
		t.Fatalf("device form = %q", form)
	}
}
