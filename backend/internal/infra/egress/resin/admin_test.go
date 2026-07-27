package resin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminRotateAccountLease(t *testing.T) {
	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/platforms/"+DefaultPlatformID+"/leases":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[{"platform_id":"` + DefaultPlatformID + `","account":"grok_build_62","node_hash":"abc","node_tag":"nl","egress_ip":"1.2.3.4"}]}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/platforms/"+DefaultPlatformID+"/leases/grok_build_62":
			deleted = "grok_build_62"
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	admin := NewAdmin(Config{Enabled: true, AdminBaseURL: server.URL, AdminToken: "token"})
	if admin == nil || !admin.Enabled() {
		t.Fatal("admin disabled")
	}
	ip, hash, err := admin.RotateAccountLease(context.Background(), "grok_build_62")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "1.2.3.4" || hash != "abc" || deleted != "grok_build_62" {
		t.Fatalf("ip=%q hash=%q deleted=%q", ip, hash, deleted)
	}
}

func TestNewAdminDisabled(t *testing.T) {
	if NewAdmin(Config{Enabled: false, AdminBaseURL: "http://x", AdminToken: "t"}) != nil {
		t.Fatal("expected nil")
	}
	if NewAdmin(Config{Enabled: true, AdminBaseURL: "", AdminToken: "t"}) != nil {
		t.Fatal("expected nil without base")
	}
}
