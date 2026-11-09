package admin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestListAuditFiltered(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, username := range []string{"alice", "bob"} {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO users(id,username,display_name,role,status) VALUES(?,?,?,'admin','active')`, username, username, username); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []struct{ actor, action, target string }{
		{"alice", "user.update", "user"},
		{"bob", "user.disable", "user"},
		{"alice", "device.create", "device"},
	} {
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO audit_events(actor_id,action,target_type,target_id,before_json,after_json) VALUES(?,?,?,?,?,?)`, e.actor, e.action, e.target, "target", `{}`, `{"username":"safe","password":"secret"}`); err != nil {
			t.Fatal(err)
		}
	}
	checks := []struct {
		actor, action, target string
		want                  int
	}{
		{"alice", "", "", 2},
		{"", "user.disable", "", 1},
		{"", "", "device", 1},
		{"alice", "user.disable", "user", 0},
	}
	for _, check := range checks {
		got, err := s.ListAuditFiltered(ctx, 0, 0, check.actor, check.action, check.target)
		if err != nil || len(got) != check.want {
			t.Fatalf("filters %+v: got %d, err %v; want %d", check, len(got), err, check.want)
		}
	}
	page, err := s.ListAuditFiltered(ctx, 0, 1, "", "", "")
	if err != nil || len(page) != 1 {
		t.Fatalf("page bound: %d, %v", len(page), err)
	}
	next, err := s.ListAuditFiltered(ctx, page[0].ID, 100, "", "", "")
	if err != nil || len(next) != 2 {
		t.Fatalf("cursor: %d, %v", len(next), err)
	}
	if _, err := s.ListAuditFiltered(ctx, 0, 101, "", "", ""); err == nil {
		t.Fatal("accepted oversized page")
	}
	if _, err := s.ListAuditFiltered(ctx, 0, 1, strings.Repeat("a", 129), "", ""); err == nil {
		t.Fatal("accepted oversized filter")
	}
	b, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") {
		t.Fatalf("secret leaked: %s", b)
	}
}
