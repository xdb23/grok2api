package account

import (
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestPreserveActiveQuotaWindowsUntilReset(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour)
	past := now.Add(-time.Second)
	incoming := []accountdomain.QuotaWindow{{Mode: "console", Remaining: 1, Total: 1}}

	// Active cooldown (remaining=0, future reset) must survive refresh so we do not reopen early.
	active := preserveActiveQuotaWindows([]accountdomain.QuotaWindow{{Mode: "console", Remaining: 0, Total: 1, ResetAt: &future}}, incoming, now)
	if len(active) != 1 || active[0].Remaining != 0 {
		t.Fatalf("active cooldown window = %#v", active)
	}

	// Expired cooldown reopens to the healthy open flag.
	expired := preserveActiveQuotaWindows([]accountdomain.QuotaWindow{{Mode: "console", Remaining: 0, Total: 1, ResetAt: &past}}, incoming, now)
	if len(expired) != 1 || expired[0].Remaining != 1 {
		t.Fatalf("expired window = %#v", expired)
	}
}
