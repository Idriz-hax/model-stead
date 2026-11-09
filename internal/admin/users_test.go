package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/Idriz-hax/model-stead/internal/database"
)

func TestUserLifecycle(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/users.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('admin','root','Root','admin','active')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO scheduler_settings(id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES(1,'weighted_round_robin',4,1,10,2,20,60,1,5)`)
	if err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "admin", User{Username: "new.user", DisplayName: "New User", Role: "user", SchedulingWeight: 1}, "temporary password")
	if err != nil {
		t.Fatal(err)
	}
	if !u.MustChangePassword {
		t.Fatal("temporary password flag missing")
	}
	var hash string
	var must int
	if err = db.QueryRow(`SELECT password_hash,must_change_password FROM users WHERE id=?`, u.ID).Scan(&hash, &must); err != nil {
		t.Fatal(err)
	}
	ok, err := auth.VerifyPassword(hash, "temporary password")
	if err != nil || !ok || must != 1 || strings.Contains(hash, "temporary password") {
		t.Fatalf("hash/flag invalid: %v %d", err, must)
	}
	if _, err = s.CreateUser(ctx, "admin", User{Username: "NEW.USER", DisplayName: "Dup", Role: "user", SchedulingWeight: 1}, "temporary password"); err == nil {
		t.Fatal("case-insensitive duplicate accepted")
	}
	if err = s.DisableUser(ctx, "admin", "admin"); err == nil {
		t.Fatal("disabled last admin")
	}
	if err = s.DisableUser(ctx, "admin", u.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.EnableUser(ctx, "admin", u.ID); err != nil {
		t.Fatal(err)
	}
}

func TestListUsersPage(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/users-page.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range [][3]string{{"a", "alpha", "Alpha"}, {"b", "bravo", "Bravo"}, {"c", "charlie", "Charlie"}} {
		if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES(?,?,?,'user','active')`, row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{DB: db}
	page, count, err := s.ListUsersPage(context.Background(), "br", 1, 0)
	if err != nil || count != 1 || len(page) != 1 || page[0].Username != "bravo" {
		t.Fatalf("page=%+v count=%d err=%v", page, count, err)
	}
	page, count, err = s.ListUsersPage(context.Background(), "", 1, 1)
	if err != nil || count != 3 || len(page) != 1 || page[0].Username != "bravo" {
		t.Fatalf("page=%+v count=%d err=%v", page, count, err)
	}
	if _, _, err = s.ListUsersPage(context.Background(), "", 101, 0); err == nil {
		t.Fatal("accepted oversized page")
	}
	if _, _, err = s.ListUsersPage(context.Background(), "", 10, -1); err == nil {
		t.Fatal("accepted negative offset")
	}
}

func TestGetUserAndLiteralSearch(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/user-search.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, row := range [][3]string{{"one", "literal%name", "Percent"}, {"two", "literalXname", "Underscore"}, {"three", "under_score", "Other"}} {
		if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES(?,?,?,'user','active')`, row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}
	s := Store{DB: db}
	got, err := s.GetUser(context.Background(), "three")
	if err != nil || got.Username != "under_score" {
		t.Fatalf("user=%+v err=%v", got, err)
	}
	for _, query := range []struct{ search, want string }{{"%", "literal%name"}, {"_", "under_score"}} {
		users, count, err := s.ListUsersPage(context.Background(), query.search, 50, 0)
		if err != nil || count != 1 || len(users) != 1 || users[0].Username != query.want {
			t.Fatalf("search %q: users=%+v count=%d err=%v", query.search, users, count, err)
		}
	}
}
