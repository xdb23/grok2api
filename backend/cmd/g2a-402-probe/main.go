package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/security"
	_ "modernc.org/sqlite"
	"golang.org/x/net/proxy"
)

func main() {
	key := os.Getenv("G2A_CRED_KEY")
	dbPath := os.Getenv("G2A_DB")
	cipher, err := security.NewCipher(key)
	must(err)
	db, err := sql.Open("sqlite", dbPath)
	must(err)
	defer db.Close()

	var encProxy string
	_ = db.QueryRow(`SELECT encrypted_proxy_url FROM egress_nodes WHERE id=1`).Scan(&encProxy)
	proxyTpl := ""
	if encProxy != "" {
		p, err := cipher.Decrypt(encProxy)
		must(err)
		proxyTpl = p
	}

	for _, id := range []int{62, 68} {
		var email, provider, clientID, encPrimary, encRefresh, egressIdent string
		var expires sql.NullTime
		err := db.QueryRow(`
			SELECT a.email, a.provider, IFNULL(c.client_id,''), IFNULL(p.egress_identity,''),
			       c.encrypted_primary, c.encrypted_refresh, c.expires_at
			FROM provider_accounts a
			JOIN account_credentials c ON c.account_id=a.id
			LEFT JOIN account_provider_links l ON l.build_account_id=a.id
			LEFT JOIN web_account_profiles p ON p.account_id=l.web_account_id
			WHERE a.id=?`, id).Scan(&email, &provider, &clientID, &egressIdent, &encPrimary, &encRefresh, &expires)
		if err != nil {
			err = db.QueryRow(`
				SELECT a.email, a.provider, IFNULL(c.client_id,''), c.encrypted_primary, c.encrypted_refresh, c.expires_at
				FROM provider_accounts a JOIN account_credentials c ON c.account_id=a.id WHERE a.id=?`, id).
				Scan(&email, &provider, &clientID, &encPrimary, &encRefresh, &expires)
			egressIdent = ""
			must(err)
		}
		token, err := cipher.Decrypt(encPrimary)
		must(err)
		refresh := ""
		if strings.TrimSpace(encRefresh) != "" {
			refresh, _ = cipher.Decrypt(encRefresh)
		}
		identity := strings.TrimSpace(egressIdent)
		if identity == "" {
			identity = provider + "_" + strconv.Itoa(id)
		}
		expNote := "null"
		if expires.Valid {
			expNote = expires.Time.UTC().Format(time.RFC3339)
			if time.Now().UTC().After(expires.Time) {
				expNote += " EXPIRED"
			}
		}
		fmt.Printf("\n=== account=%d email=%s identity=%s expires=%s refresh_len=%d ===\n", id, email, identity, expNote, len(refresh))

		// refresh if expired or near expiry
		if refresh != "" && ( !expires.Valid || time.Now().UTC().After(expires.Time.Add(-2*time.Minute)) ) {
			newTok, newExp, rerr := refreshToken(refresh, clientID)
			if rerr != nil {
				fmt.Printf("  refresh_err=%v\n", rerr)
			} else {
				token = newTok
				fmt.Printf("  refreshed ok new_expires=%s token_len=%d\n", newExp, len(token))
			}
		}

		// A: direct
		probe("A_direct", "", "", token)
		// B: resin sticky identity
		if proxyTpl != "" {
			p1 := strings.ReplaceAll(proxyTpl, "{account}", identity)
			probe("B_resin_sticky", p1, identity, token)
			// C: resin with a fresh random-ish identity (simulate "换 IP 账号槽")
			fresh := fmt.Sprintf("probe_%d_%d", id, time.Now().Unix()%100000)
			p2 := strings.ReplaceAll(proxyTpl, "{account}", fresh)
			probe("C_resin_fresh_slot", p2, fresh, token)
		}
	}
}

func refreshToken(refresh, clientID string) (string, string, error) {
	if clientID == "" {
		clientID = "cli" // fallback; real may be in token
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", clientID)
	// try common oauth endpoints used by g2a
	endpoints := []string{
		"https://accounts.x.ai/oauth/token",
		"https://auth.x.ai/oauth/token",
		"https://cli-auth.x.ai/oauth/token",
	}
	var last error
	for _, ep := range endpoints {
		req, _ := http.NewRequest("POST", ep, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "grok-shell/0.2.111 (linux; x86_64)")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			last = err
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4000))
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			last = fmt.Errorf("%s -> %d %s", ep, resp.StatusCode, string(b)[:min(200, len(b))])
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			last = err
			continue
		}
		at, _ := m["access_token"].(string)
		if at == "" {
			last = fmt.Errorf("no access_token from %s", ep)
			continue
		}
		exp := ""
		if v, ok := m["expires_in"].(float64); ok {
			exp = time.Now().UTC().Add(time.Duration(v) * time.Second).Format(time.RFC3339)
		}
		return at, exp, nil
	}
	return "", "", last
}

func probe(label, proxyURL, identity, token string) {
	client, note, err := makeClient(proxyURL)
	if err != nil {
		fmt.Printf("  [%s] client_err=%v\n", label, err)
		return
	}
	if note != "" {
		fmt.Printf("  [%s] %s identity=%s\n", label, note, identity)
	}
	if ip := fetchIP(client); ip != "" {
		fmt.Printf("  [%s] exit_ip=%s\n", label, ip)
	} else {
		fmt.Printf("  [%s] exit_ip=(failed)\n", label)
	}
	body := map[string]any{"model": "grok-4.5", "input": "ping reply with ok only", "stream": false}
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(context.Background(), "POST", "https://cli-chat-proxy.grok.com/v1/responses", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.111")
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("x-grok-client-mode", "headless")
	req.Header.Set("User-Agent", "grok-shell/0.2.111 (linux; x86_64)")
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Printf("  [%s] transport_err=%v elapsed=%s\n", label, err, elapsed.Round(time.Millisecond))
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
	code := ""
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if c, ok := m["code"].(string); ok {
			code = c
		} else if e, ok := m["error"].(string); ok {
			code = e
		}
	}
	fmt.Printf("  [%s] status=%d elapsed=%s code=%q\n", label, resp.StatusCode, elapsed.Round(time.Millisecond), trim(code, 160))
}

func makeClient(proxyURL string) (*http.Client, string, error) {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	note := ""
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, "", err
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme == "socks5" || scheme == "socks5h" {
			var auth *proxy.Auth
			if u.User != nil {
				pass, _ := u.User.Password()
				auth = &proxy.Auth{User: u.User.Username(), Password: pass}
			}
			d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
			if err != nil {
				return nil, "", err
			}
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
			note = "proxy=socks5 host=" + u.Host
		} else {
			tr.Proxy = http.ProxyURL(u)
			user := ""
			if u.User != nil {
				user = u.User.Username()
			}
			note = "proxy=http host=" + u.Host + " user=" + user
		}
	}
	return &http.Client{Transport: tr, Timeout: 75 * time.Second}, note, nil
}

func fetchIP(c *http.Client) string {
	req, _ := http.NewRequest("GET", "https://api.ipify.org", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return string(bytes.TrimSpace(b))
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
