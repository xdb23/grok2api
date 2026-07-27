// Package resin talks to a local Resin sticky-proxy admin API.
//
// HTTPS CONNECT tunnels hide upstream HTTP 402 bodies from Resin, so G2A must
// explicitly release sticky leases when spending-limit 402 is observed on a
// Build account. ReleaseLease is the supported way to force Resin to assign a
// different exit node for the same account identity on the next CONNECT.
package resin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultPlatformID is Resin's built-in "Default" platform (username prefix Default.).
const DefaultPlatformID = "00000000-0000-0000-0000-000000000000"

// Config configures the Resin admin client.
type Config struct {
	// Enabled turns on admin-side lease release. Proxy traffic still uses egress_nodes.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// AdminBaseURL is the Resin HTTP management base, e.g. http://127.0.0.1:3030.
	AdminBaseURL string `yaml:"adminBaseURL" json:"adminBaseURL"`
	// AdminToken is RESIN_ADMIN_TOKEN (Bearer).
	AdminToken string `yaml:"adminToken" json:"adminToken"`
	// PlatformID defaults to DefaultPlatformID when empty.
	PlatformID string `yaml:"platformID" json:"platformID"`
	// Timeout for admin HTTP calls. Zero uses 5s.
	Timeout time.Duration `yaml:"timeout" json:"timeout"`
}

// Lease describes a sticky account → exit binding.
type Lease struct {
	PlatformID string `json:"platform_id"`
	Account    string `json:"account"`
	NodeHash   string `json:"node_hash"`
	NodeTag    string `json:"node_tag"`
	EgressIP   string `json:"egress_ip"`
	Expiry     string `json:"expiry"`
}

// Admin is a minimal Resin management client used by the gateway.
type Admin struct {
	base       string
	token      string
	platformID string
	client     *http.Client
}

// NewAdmin returns nil when cfg is disabled or incomplete.
func NewAdmin(cfg Config) *Admin {
	if !cfg.Enabled {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.AdminBaseURL), "/")
	token := strings.TrimSpace(cfg.AdminToken)
	if base == "" || token == "" {
		return nil
	}
	platformID := strings.TrimSpace(cfg.PlatformID)
	if platformID == "" {
		platformID = DefaultPlatformID
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Admin{
		base:       base,
		token:      token,
		platformID: platformID,
		client:     &http.Client{Timeout: timeout},
	}
}

// Enabled reports whether the client is usable.
func (a *Admin) Enabled() bool { return a != nil && a.base != "" && a.token != "" }

// GetLease returns the sticky lease for account, or nil when unbound.
func (a *Admin) GetLease(ctx context.Context, account string) (*Lease, error) {
	if !a.Enabled() {
		return nil, nil
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil, nil
	}
	// List filter is not always available; fetch by scanning a bounded page then exact path if needed.
	// Prefer direct GET of single lease via list with account match from full list offset 0 limit high.
	path := fmt.Sprintf("/api/v1/platforms/%s/leases?limit=500", url.PathEscape(a.platformID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+path, nil)
	if err != nil {
		return nil, err
	}
	a.authorize(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("resin list leases: HTTP %d %s", resp.StatusCode, truncate(string(body), 160))
	}
	var payload struct {
		Items []Lease `json:"items"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	for i := range payload.Items {
		if payload.Items[i].Account == account {
			lease := payload.Items[i]
			return &lease, nil
		}
	}
	// Fallback: paginate a bit if total is large (best-effort).
	return nil, nil
}

// ReleaseLease drops the sticky binding so the next CONNECT gets a new exit.
// Missing leases are treated as success (idempotent).
func (a *Admin) ReleaseLease(ctx context.Context, account string) error {
	if !a.Enabled() {
		return nil
	}
	account = strings.TrimSpace(account)
	if account == "" {
		return nil
	}
	path := fmt.Sprintf("/api/v1/platforms/%s/leases/%s", url.PathEscape(a.platformID), url.PathEscape(account))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, a.base+path, nil)
	if err != nil {
		return err
	}
	a.authorize(req)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	// 204 No Content and 404 Not Found are both fine.
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusOK {
		return nil
	}
	return fmt.Errorf("resin release lease: HTTP %d %s", resp.StatusCode, truncate(string(body), 160))
}

// RotateAccountLease snapshots the old binding, releases it, and returns the old egress IP/node.
func (a *Admin) RotateAccountLease(ctx context.Context, account string) (oldEgressIP, oldNodeHash string, err error) {
	if !a.Enabled() {
		return "", "", nil
	}
	old, _ := a.GetLease(ctx, account)
	if old != nil {
		oldEgressIP, oldNodeHash = old.EgressIP, old.NodeHash
	}
	if err := a.ReleaseLease(ctx, account); err != nil {
		return oldEgressIP, oldNodeHash, err
	}
	return oldEgressIP, oldNodeHash, nil
}

func (a *Admin) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
