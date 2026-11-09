package admin

import (
	"context"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/database"
)

func testStore(t *testing.T) Store {
	t.Helper()
	db, err := database.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err = db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('admin','admin','Administrator','admin','active')`); err != nil {
		t.Fatal(err)
	}
	return Store{DB: db}
}
func testSettings() SchedulerSettings {
	return SchedulerSettings{Policy: "weighted_round_robin", GlobalCapacity: 4, DefaultWeight: 1, MaxQueued: 100, MaxActiveContended: 2, GlobalMaxQueued: 500, MaxWait: 30 * time.Minute, WorkConserving: true, CancellationGrace: 30 * time.Second}
}

func TestSettingsSeedAndRead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	want := testSettings()
	if err := s.SeedSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	want.GlobalCapacity = 9
	if err := s.SeedSettings(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.GlobalCapacity != 4 || got.MaxWait != 30*time.Minute {
		t.Fatalf("got %+v", got)
	}
}

func TestUpdateSettingsCommitsSettingsAndAudit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	initial := testSettings()
	if err := s.SeedSettings(ctx, initial); err != nil {
		t.Fatal(err)
	}
	want := initial
	want.GlobalCapacity = 8
	if err := s.UpdateSettings(ctx, "admin", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.GlobalCapacity != want.GlobalCapacity {
		t.Fatalf("capacity = %d, want %d", got.GlobalCapacity, want.GlobalCapacity)
	}
	events, err := s.ListAudit(ctx, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "scheduler.update" || events[0].After.Scheduler == nil || events[0].After.Scheduler.GlobalCapacity != want.GlobalCapacity {
		t.Fatalf("unexpected audit events: %+v", events)
	}
}
func TestValidate(t *testing.T) {
	if err := ValidateWeight(101); err == nil {
		t.Fatal("accepted invalid weight")
	}
	if err := ValidateCapacity(65); err == nil {
		t.Fatal("accepted invalid capacity")
	}
	if err := ValidateUsername("bad name"); err == nil {
		t.Fatal("accepted invalid name")
	}
	if err := ValidateIP("192.0.2.0/24"); err == nil {
		t.Fatal("accepted CIDR")
	}
	if err := ValidateSettings(testSettings()); err != nil {
		t.Fatal(err)
	}
	fractional := testSettings()
	fractional.MaxWait = 1500 * time.Millisecond
	if err := ValidateSettings(fractional); err == nil {
		t.Fatal("accepted fractional-second max wait")
	}
}
func TestAuditAtomicRedactedAndPaginated(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	f := 2.0
	err := s.AuditedExec(ctx, "admin", "user.update", "user", "u1", `INSERT INTO users(id,username,display_name,role,status) VALUES('u1','user1','User','user','active')`, nil, Redacted{}, Redacted{Username: "user1", SchedulingWeight: &f})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AuditedExec(ctx, "admin", "user.update", "user", "u2", `INSERT INTO users(id,username,display_name,role,status) VALUES('u1','other','User','user','active')`, nil, Redacted{}, Redacted{}); err == nil {
		t.Fatal("expected mutation failure")
	}
	rows, err := s.ListAudit(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].After.Username != "user1" {
		t.Fatalf("unexpected events: %+v", rows)
	}
	if err = s.AuditedExec(ctx, "admin", "user.update", "user", "u1", `UPDATE users SET display_name='Changed' WHERE id='u1'`, nil, Redacted{}, Redacted{}); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAudit(ctx, rows[0].ID, 100)
	if err != nil || len(rows) != 0 {
		t.Fatalf("cursor pagination: %v %+v", err, rows)
	}
	if _, err = s.ListAudit(ctx, 0, 101); err == nil {
		t.Fatal("accepted oversized page")
	}
}

func TestAuditEventsAreAppendOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.AuditedExec(ctx, "admin", "user.update", "user", "u1", `INSERT INTO users(id,username,display_name,role,status) VALUES('u1','user1','User','user','active')`, nil, Redacted{}, Redacted{Username: "user1"}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.DB.QueryRowContext(ctx, `SELECT id FROM audit_events`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`UPDATE audit_events SET action='changed' WHERE id=?`, `DELETE FROM audit_events WHERE id=?`} {
		if _, err := s.DB.ExecContext(ctx, statement, id); err == nil {
			t.Fatalf("expected audit immutability trigger to reject %q", statement)
		}
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE id=?`, id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit row count = %d, err %v; want 1", count, err)
	}
}
