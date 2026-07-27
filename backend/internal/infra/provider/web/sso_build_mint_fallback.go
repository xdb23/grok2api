package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

const (
	defaultSSOMintImage   = "grok-build-auth-theyka-speed:local"
	defaultSSOMintNetwork = "resin_default"
)

// convertViaProtocolMint runs the proven theyka mint_with_sso_protocol path inside Docker.
// Used as a fallback when the pure-Go Device Flow hits accounts.x.ai HTTP 403.
func convertViaProtocolMint(ctx context.Context, sso, proxyURL, email, name string) (provider.CredentialSeed, error) {
	if strings.TrimSpace(sso) == "" {
		return provider.CredentialSeed{}, fmt.Errorf("protocol mint: empty SSO")
	}
	proxyURL = rewriteProxyForDockerMint(strings.TrimSpace(proxyURL))
	if proxyURL == "" {
		return provider.CredentialSeed{}, fmt.Errorf("protocol mint: empty proxy URL")
	}
	image := strings.TrimSpace(os.Getenv("G2A_SSO_MINT_IMAGE"))
	if image == "" {
		image = defaultSSOMintImage
	}
	network := strings.TrimSpace(os.Getenv("G2A_SSO_MINT_NETWORK"))
	if network == "" {
		network = defaultSSOMintNetwork
	}
	if disabled := strings.TrimSpace(os.Getenv("G2A_SSO_MINT_FALLBACK")); disabled == "0" || strings.EqualFold(disabled, "false") {
		return provider.CredentialSeed{}, fmt.Errorf("protocol mint fallback disabled")
	}

	workDir, err := os.MkdirTemp("", "g2a-sso-mint-*")
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	defer os.RemoveAll(workDir)
	_ = os.Chmod(workDir, 0o700)

	meta := map[string]string{"email": email, "sso": sso, "proxy_url": proxyURL}
	metaBytes, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(workDir, "meta.json"), metaBytes, 0o600); err != nil {
		return provider.CredentialSeed{}, err
	}
	script := `import json, time, sys, base64
from pathlib import Path
sys.path[:0]=['/app/registrar_lib','/app']
from grok_platform.protocol_mint import mint_with_sso_protocol
meta=json.loads(Path('/work/meta.json').read_text())
t0=time.time()
try:
    tokens=mint_with_sso_protocol(meta['sso'], meta['proxy_url'])
    def claims(tok):
        parts=tok.split('.')
        if len(parts)<2: return {}
        pad='='*((4-len(parts[1])%4)%4)
        return json.loads(base64.urlsafe_b64decode(parts[1]+pad))
    ac=claims(tokens.access_token)
    out={
        'ok': True,
        'seconds': round(time.time()-t0, 2),
        'access_token': tokens.access_token,
        'refresh_token': tokens.refresh_token,
        'id_token': tokens.id_token or '',
        'expires_in': int(tokens.expires_in or 21600),
        'sub': str(ac.get('sub') or ac.get('principal_id') or ''),
        'email': str(ac.get('email') or meta.get('email') or ''),
        'team_id': str(ac.get('team_id') or ''),
    }
    Path('/work/result.json').write_text(json.dumps(out))
    print('OK', out['seconds'], flush=True)
except Exception as e:
    Path('/work/result.json').write_text(json.dumps({'ok': False, 'error': f'{type(e).__name__}: {e}'}))
    print('FAIL', type(e).__name__, str(e)[:500], flush=True)
    raise SystemExit(2)
`
	if err := os.WriteFile(filepath.Join(workDir, "mint_one.py"), []byte(script), 0o600); err != nil {
		return provider.CredentialSeed{}, err
	}

	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "docker", "run", "--rm",
		"--network", network,
		"-v", workDir+":/work",
		"--entrypoint", "python3",
		image,
		"/work/mint_one.py",
	)
	output, err := cmd.CombinedOutput()
	resultPath := filepath.Join(workDir, "result.json")
	raw, readErr := os.ReadFile(resultPath)
	if readErr != nil {
		return provider.CredentialSeed{}, fmt.Errorf("protocol mint: no result (%v): %s", err, strings.TrimSpace(string(output)))
	}
	var result struct {
		OK           bool   `json:"ok"`
		Error        string `json:"error"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
		Sub          string `json:"sub"`
		Email        string `json:"email"`
		TeamID       string `json:"team_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return provider.CredentialSeed{}, fmt.Errorf("protocol mint: bad result json: %w", err)
	}
	if !result.OK || result.AccessToken == "" || result.RefreshToken == "" {
		msg := result.Error
		if msg == "" {
			msg = strings.TrimSpace(string(output))
		}
		return provider.CredentialSeed{}, fmt.Errorf("protocol mint failed: %s", msg)
	}
	expiresIn := result.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	emailOut := firstValue(result.Email, email)
	nameOut := strings.TrimSpace(name)
	if nameOut == "" {
		nameOut = "Grok Web account"
	}
	return provider.CredentialSeed{
		Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
		Name: firstValue(emailOut, nameOut+" Build", result.Sub, "Grok Build account"),
		Email: emailOut, UserID: result.Sub, TeamID: result.TeamID,
		SourceKey: "sso-build:" + security.HashToken(result.AccessToken), OIDCClientID: ssoBuildClientID,
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		ExpiresAt: time.Now().UTC().Add(time.Duration(expiresIn) * time.Second),
	}, nil
}

func rewriteProxyForDockerMint(proxy string) string {
	if proxy == "" {
		return proxy
	}
	replacer := strings.NewReplacer(
		"127.0.0.1:3030", "resin:2260",
		"localhost:3030", "resin:2260",
		"@127.0.0.1:", "@resin:",
		"@localhost:", "@resin:",
	)
	return replacer.Replace(proxy)
}

func isConvertPortal403(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 403") || strings.Contains(msg, "xai oauth http 403")
}
