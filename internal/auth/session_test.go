package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/database"
)

func TestSessionLifecycleAndDisabledUsers(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('u','user','User','user','active')`); err != nil {
		t.Fatal(err)
	}
	s, err := CreateSession(ctx, db, "u", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if s.Token == "" || s.CSRFToken == "" || s.ID == "" {
		t.Fatalf("incomplete session: %+v", s)
	}
	var stored string
	if err := db.QueryRow(`SELECT token_hash FROM sessions WHERE id=?`, s.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(s.Token))
	if stored != hex.EncodeToString(sum[:]) {
		t.Fatal("database does not contain token SHA256")
	}
	got, err := LookupSession(ctx, db, s.Token)
	if err != nil || got.ID != s.ID || got.CSRFToken != s.CSRFToken {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	if err := RevokeSession(ctx, db, s.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupSession(ctx, db, s.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("revoked lookup error = %v", err)
	}
	if _, err := CreateSession(ctx, db, "missing", time.Hour); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("missing user create error = %v", err)
	}
	if _, err := db.Exec(`UPDATE users SET status='disabled' WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateSession(ctx, db, "u", time.Hour); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("disabled user create error = %v", err)
	}
}

func TestSessionRejectsExpired(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "expired.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	_, err = db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('u','user','User','user','active')`)
	if err != nil {
		t.Fatal(err)
	}
	s, err := CreateSession(ctx, db, "u", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE sessions SET expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupSession(ctx, db, s.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("expired lookup error = %v", err)
	}
}
