package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/egress/resin"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	_ "modernc.org/sqlite"
)

func main() {
	key := os.Getenv("G2A_CRED_KEY")
	dbPath := os.Getenv("G2A_DB")
	adminTok := os.Getenv("RESIN_ADMIN")
	cipher, err := security.NewCipher(key)
	must(err)
	db, err := sql.Open("sqlite", dbPath)
	must(err)
	defer db.Close()
	var encPrimary, encProxy string
	must(db.QueryRow(`SELECT c.encrypted_primary FROM account_credentials c WHERE c.account_id=62`).Scan(&encPrimary))
	must(db.QueryRow(`SELECT encrypted_proxy_url FROM egress_nodes WHERE id=1`).Scan(&encProxy))
	token, err := cipher.Decrypt(encPrimary)
	must(err)
	proxyTpl, err := cipher.Decrypt(encProxy)
	must(err)
	admin := resin.NewAdmin(resin.Config{Enabled: true, AdminBaseURL: "http://127.0.0.1:3030", AdminToken: adminTok})
	identity := "grok_build_62"
	maxRot := 3
	_, _, _ = admin.RotateAccountLease(context.Background(), identity)

	for n := 0; n <= maxRot; n++ {
		proxyURL := strings.ReplaceAll(proxyTpl, "{account}", identity)
		u, err := url.Parse(proxyURL)
		must(err)
		tr := &http.Transport{Proxy: http.ProxyURL(u), ResponseHeaderTimeout: 45 * time.Second}
		client := &http.Client{Transport: tr, Timeout: 60 * time.Second}
		ip := fetchIP(client)
		status, code := callUpstream(client, token)
		fmt.Printf("try=%d exit_ip=%s status=%d code=%s\n", n, ip, status, code)
		if status != 402 {
			fmt.Println("STOP: non-402 (gateway would return success / other error)")
			break
		}
		if n == maxRot {
			fmt.Println("exhausted rotations -> soft-cool spending_limit 1h")
			break
		}
		oldIP, oldHash, err := admin.RotateAccountLease(context.Background(), identity)
		fmt.Printf("  rotate#%d old_ip=%q hash=%q err=%v\n", n+1, oldIP, oldHash, err)
		must(err)
	}
	now := time.Now().UTC()
	next := now.Add(time.Hour)
	_, _ = db.Exec(`DELETE FROM account_quota_recovery WHERE account_id=62`)
	_, err = db.Exec(`INSERT INTO account_quota_recovery(account_id,kind,status,confirmed_used,confirmed_limit,exhausted_at,next_probe_at,last_confirmed_at,updated_at)
		VALUES(62,'spending_limit','exhausted',0,0,?,?,?,?)`, now, next, now, now)
	must(err)
	var kind, status, nextProbe string
	must(db.QueryRow(`SELECT kind,status,next_probe_at FROM account_quota_recovery WHERE account_id=62`).Scan(&kind, &status, &nextProbe))
	fmt.Printf("recovery kind=%s status=%s next_probe_at=%s\n", kind, status, nextProbe)
	var cnt int
	must(db.QueryRow(`SELECT count(*) FROM account_quota_recovery WHERE kind='spending_limit'`).Scan(&cnt))
	fmt.Printf("filter spending_limit count=%d\n", cnt)
	must(db.QueryRow(`SELECT count(*) FROM account_quota_recovery WHERE kind='free'`).Scan(&cnt))
	fmt.Printf("filter free count=%d\n", cnt)
}

func callUpstream(client *http.Client, token string) (int, string) {
	body, _ := json.Marshal(map[string]any{"model": "grok-4.5", "input": "ping ok only", "stream": false})
	req, _ := http.NewRequest("POST", "https://cli-chat-proxy.grok.com/v1/responses", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.111")
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("x-grok-client-mode", "headless")
	req.Header.Set("User-Agent", "grok-shell/0.2.111 (linux; x86_64)")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
	code := ""
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if c, ok := m["code"].(string); ok {
			code = c
		}
	}
	return resp.StatusCode, code
}

func fetchIP(c *http.Client) string {
	req, _ := http.NewRequest("GET", "https://api.ipify.org", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
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
