package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

func TestAccountRequestStatsAsOfIncrementsPerRow(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "asof.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewAuditRepository(database)
	now := time.Now().UTC()
	accountID := uint64(7)
	for i, ok := range []bool{true, false, true} {
		status, code := 200, ""
		if !ok {
			status, code = 500, "upstream_error"
		}
		if err := repo.Create(ctx, audit.Record{
			RequestID: "req-" + string(rune('a'+i)), ClientKeyID: 1, ModelRouteID: 1,
			Provider: "grok_build", Operation: audit.OperationChat, UsageSource: audit.UsageSourceNone,
			AccountID: &accountID, AccountName: "acc", StatusCode: status, ErrorCode: code,
			DurationMS: 100, CreatedAt: now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	list, _, err := repo.List(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list=%d", len(list))
	}
	// list is desc created_at: third, second, first
	ids := []uint64{list[0].ID, list[1].ID, list[2].ID}
	stats, err := repo.AccountRequestStatsAsOf(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	// oldest (list[2]): 1 success
	if s := stats[list[2].ID]; s.Requests != 1 || s.Successes != 1 || s.Failures != 0 {
		t.Fatalf("oldest=%#v", s)
	}
	// middle fail: 2 total, 1 success, 1 fail
	if s := stats[list[1].ID]; s.Requests != 2 || s.Successes != 1 || s.Failures != 1 {
		t.Fatalf("middle=%#v", s)
	}
	// newest success: 3 total, 2 success, 1 fail
	if s := stats[list[0].ID]; s.Requests != 3 || s.Successes != 2 || s.Failures != 1 {
		t.Fatalf("newest=%#v", s)
	}
}
