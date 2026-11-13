package admin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestCreateFirstAdminRejectsExistingAdmin(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/existing-admin.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO scheduler_settings(id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES(1,'weighted_round_robin',4,1,10,2,20,60,1,5)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('existing','existing','Existing','admin','active')`); err != nil {
		t.Fatal(err)
	}
	_, err = (Store{DB: db}).CreateFirstAdmin(context.Background(), User{Username: "owner", DisplayName: "Owner", Role: "admin", SchedulingWeight: 1}, "a sufficiently long password")
	if err == nil {
		t.Fatal("created a first admin when an active admin already existed")
	}
}

func TestCreateFirstAdminConcurrentConnections(t *testing.T) {
	path := t.TempDir() + "/concurrent-first-admin.db"
	db1, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	db2, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if db1 == db2 {
		t.Fatal("database.Open returned the same connection handle")
	}
	if _, err = db1.Exec(`INSERT INTO scheduler_settings(id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES(1,'weighted_round_robin',4,1,10,2,20,60,1,5)`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for i, db := range []*sql.DB{db1, db2} {
		go func(i int, db *sql.DB) {
			<-start
			_, err := (Store{DB: db}).CreateFirstAdmin(ctx, User{Username: fmt.Sprintf("owner%d", i), DisplayName: fmt.Sprintf("Owner %d", i), Role: "admin", SchedulingWeight: 1}, "a sufficiently long password")
			results <- err
		}(i, db)
	}
	close(start)
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims=%d, want 1", successes)
	}
	var count int
	if err = db1.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("admin count=%d, want 1", count)
	}
}

func TestCreateFirstAdmin(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/first-admin.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO scheduler_settings(id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES(1,'weighted_round_robin',4,1,10,2,20,60,1,5)`); err != nil {
		t.Fatal(err)
	}
	s := Store{DB: db}
	ctx := context.Background()
	u, err := s.CreateFirstAdmin(ctx, User{Username: "owner", DisplayName: "Owner", Role: "admin", SchedulingWeight: 1}, "a sufficiently long password")
	if err != nil || u.ID == "" || u.Role != "admin" || u.Status != "active" || u.MustChangePassword {
		t.Fatalf("first admin=%+v err=%v", u, err)
	}
	var hash string
	if err = db.QueryRow(`SELECT password_hash FROM users WHERE id=?`, u.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	ok, err := auth.VerifyPassword(hash, "a sufficiently long password")
	if err != nil || !ok || hash == "a sufficiently long password" {
		t.Fatalf("password hash invalid: %v", err)
	}
	var events int
	if err = db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='admin.bootstrap'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("bootstrap events=%d", events)
	}
	if _, err = s.CreateFirstAdmin(ctx, User{Username: "second", DisplayName: "Second", Role: "admin", SchedulingWeight: 1}, "another sufficiently long password"); err == nil {
		t.Fatal("created a second initial administrator")
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
