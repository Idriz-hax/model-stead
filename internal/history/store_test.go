package history

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/database"
)

func fixture(t *testing.T) (*Store, context.Context) {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/history.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, id := range []string{"u1", "u2"} {
		if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES(?,?,?,'user','active')`, id, id, id); err != nil {
			t.Fatal(err)
		}
	}
	return &Store{DB: db}, context.Background()
}

func TestOwnerScopedCreateUpdateAndList(t *testing.T) {
	s, ctx := fixture(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.Create(ctx, Record{RequestID: "r1", UserID: "u1", Endpoint: "/v1/chat/completions", EnqueuedAt: now, Outcome: "queued", UsageStatus: "unavailable"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLifecycle(ctx, "u1", "r1", Lifecycle{Outcome: "completed", CompletedAt: &now}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateLifecycle(ctx, "u2", "r1", Lifecycle{Outcome: "rejected"}); err == nil {
		t.Fatal("cross-owner update succeeded")
	}
	rows, total, err := s.ListPage(ctx, "u2", Query{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(rows) != 0 {
		t.Fatalf("cross-owner rows: %d", total)
	}
	rows, total, err = s.ListPage(ctx, "u1", Query{})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].Outcome != "completed" {
		t.Fatalf("rows=%+v total=%d err=%v", rows, total, err)
	}
}

func TestUsageNullableAggregationAndRecoveryPurge(t *testing.T) {
	s, ctx := fixture(t)
	start := time.Now().UTC().Add(-4 * time.Hour)
	for i, v := range []Record{
		{RequestID: "a", UserID: "u1", Endpoint: "/v1/completions", EnqueuedAt: start, Outcome: "completed", InputTokens: ptr(int64(4)), UsageStatus: "reported"},
		{RequestID: "b", UserID: "u1", Endpoint: "/v1/messages", EnqueuedAt: start.Add(time.Hour), Outcome: "active", OutputTokens: ptr(int64(3)), UsageStatus: "partial"},
		{RequestID: "c", UserID: "u1", Endpoint: "/v1/responses", EnqueuedAt: start.Add(2 * time.Hour), Outcome: "queued", UsageStatus: "unavailable"},
	} {
		if err := s.Create(ctx, v); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	usage, err := s.Usage(ctx, "u1", start.Add(-time.Hour), start.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var requests, incomplete, input, output int64
	for _, day := range usage {
		requests += day.Requests
		incomplete += day.Incomplete
		if day.InputTokens != nil {
			input += *day.InputTokens
		}
		if day.OutputTokens != nil {
			output += *day.OutputTokens
		}
	}
	if requests != 3 || input != 4 || output != 3 || incomplete != 2 {
		t.Fatalf("usage: %+v", usage)
	}
	n, err := s.RecoverInterrupted(ctx)
	if err != nil || n != 2 {
		t.Fatalf("recovered=%d err=%v", n, err)
	}
	deleted, err := s.PurgeBefore(ctx, start.Add(time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("deleted=%d err=%v", deleted, err)
	}
}

func TestAllOutcomesAndQueryBounds(t *testing.T) {
	s, ctx := fixture(t)
	for i, outcome := range []string{"queued", "active", "completed", "rejected", "timed_out", "cancelled", "upstream_error", "interrupted"} {
		if err := s.Create(ctx, Record{RequestID: string(rune('a' + i)), UserID: "u1", Endpoint: "/v1/embeddings", EnqueuedAt: time.Now(), Outcome: outcome, UsageStatus: "unavailable"}); err != nil {
			t.Fatalf("outcome %s: %v", outcome, err)
		}
	}
	for _, q := range []Query{{Limit: 101}, {Limit: -1}, {Offset: -1}, {Endpoint: "/v1/nope"}, {Outcome: "nope"}, {Search: strings.Repeat("x", 129)}} {
		if _, _, err := s.ListPage(ctx, "u1", q); err == nil {
			t.Fatalf("accepted invalid query: %+v", q)
		}
	}
	rows, total, err := s.ListPage(ctx, "u1", Query{Limit: 3, Offset: 2})
	if err != nil || total != 8 || len(rows) != 3 {
		t.Fatalf("pagination rows=%d total=%d err=%v", len(rows), total, err)
	}
}

func TestDateFilterBoundsAndUsageRequiresNonemptyRange(t *testing.T) {
	s, ctx := fixture(t)
	now := time.Now().UTC()
	tooOld := now.Add(-30*24*time.Hour - time.Second)
	tooNew := now.Add(time.Second)
	for _, q := range []Query{{From: &tooOld}, {To: &tooOld}, {From: &tooNew}, {To: &tooNew}} {
		if _, _, err := s.ListPage(ctx, "u1", q); err == nil {
			t.Fatalf("accepted out-of-window one-sided range: %+v", q)
		}
	}
	if _, err := s.Usage(ctx, "u1", now, now); err == nil {
		t.Fatal("accepted empty usage range")
	}
	tooOldUsage := now.Add(-30*24*time.Hour - time.Second)
	tooNewUsage := now.Add(time.Second)
	for _, bounds := range [][2]time.Time{{tooOldUsage, now}, {now, tooNewUsage}, {tooOldUsage, tooNewUsage}} {
		if _, err := s.Usage(ctx, "u1", bounds[0], bounds[1]); err == nil {
			t.Fatalf("accepted usage range outside retained window: %v - %v", bounds[0], bounds[1])
		}
	}
}

func TestUsageUTCDateBucketsAndPurgeCutoffIsExclusive(t *testing.T) {
	s, ctx := fixture(t)
	cutoff := time.Now().UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	for _, r := range []Record{
		{RequestID: "before", UserID: "u1", Endpoint: "/v1/completions", EnqueuedAt: cutoff.Add(-time.Millisecond), Outcome: "completed", InputTokens: ptr(int64(2)), UsageStatus: "reported"},
		{RequestID: "edge", UserID: "u1", Endpoint: "/v1/completions", EnqueuedAt: cutoff, Outcome: "completed", InputTokens: ptr(int64(5)), UsageStatus: "reported"},
		{RequestID: "nextday", UserID: "u1", Endpoint: "/v1/messages", EnqueuedAt: cutoff.Add(24 * time.Hour), Outcome: "completed", OutputTokens: ptr(int64(7)), UsageStatus: "partial"},
	} {
		if err := s.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := s.Usage(ctx, "u1", cutoff, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 || usage[0].Day != cutoff.Format("2006-01-02") || !countIs(usage[0].InputTokens, 5) || usage[0].OutputTokens != nil || usage[1].Day != cutoff.Add(24*time.Hour).Format("2006-01-02") || usage[1].InputTokens != nil || !countIs(usage[1].OutputTokens, 7) {
		t.Fatalf("UTC buckets: %+v", usage)
	}
	deleted, err := s.PurgeBefore(ctx, cutoff)
	if err != nil || deleted != 1 {
		t.Fatalf("purged=%d err=%v", deleted, err)
	}
	rows, total, err := s.ListPage(ctx, "u1", Query{})
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("cutoff row not retained: %d %v", total, err)
	}
}
func ptr[T any](v T) *T                 { return &v }
func countIs(v *int64, want int64) bool { return v != nil && *v == want }
