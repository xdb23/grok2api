package sessionidentity

import (
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestParseMarksBlockedSessionUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"blocked"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err=%v want ErrUnauthorized", err)
	}
}

func TestParseMarksUnauthenticatedSessionUnauthorized(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{"status":"unauthenticated"}`))
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err=%v want ErrUnauthorized", err)
	}
}

func TestParseMissingIdentityWithoutStatus(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(`{}`))
	if err == nil || errors.Is(err, provider.ErrUnauthorized) {
		t.Fatalf("err=%v want generic missing-identity error", err)
	}
}
