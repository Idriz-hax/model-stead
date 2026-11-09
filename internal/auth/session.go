package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidSession represents absent, expired, revoked, or inactive-user sessions.
var ErrInvalidSession = errors.New("invalid session")

// Session is a user's authenticated browser session. Token is returned only at creation.
type Session struct {
	ID        string
	UserID    string
	Token     string
	CSRFToken string
	ExpiresAt time.Time
}

// CreateSession creates a session for an active user and returns its one-time bearer token.
func CreateSession(ctx context.Context, db *sql.DB, userID string, ttl time.Duration) (Session, error) {
	if ttl <= 0 {
		return Session{}, fmt.Errorf("session ttl must be positive")
	}
	var tokenBytes, csrfBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return Session{}, fmt.Errorf("generate session token: %w", err)
	}
	if _, err := rand.Read(csrfBytes[:]); err != nil {
		return Session{}, fmt.Errorf("generate CSRF token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes[:])
	csrf := base64.RawURLEncoding.EncodeToString(csrfBytes[:])
	sum := sha256.Sum256([]byte(token))
	expires := time.Now().UTC().Add(ttl)
	id := uuid.NewString()
	result, err := db.ExecContext(ctx, `INSERT INTO sessions(id,user_id,token_hash,csrf_token,expires_at) SELECT ?,?,?,?,? WHERE EXISTS (SELECT 1 FROM users WHERE id=? AND status='active')`, id, userID, hex.EncodeToString(sum[:]), csrf, expires.Format(time.RFC3339Nano), userID)
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Session{}, fmt.Errorf("create session: %w", err)
	}
	if n != 1 {
		return Session{}, ErrInvalidSession
	}
	return Session{ID: id, UserID: userID, Token: token, CSRFToken: csrf, ExpiresAt: expires}, nil
}

// LookupSession validates a bearer token and returns its session and CSRF token.
func LookupSession(ctx context.Context, db *sql.DB, token string) (Session, error) {
	if len(token) != 43 {
		return Session{}, ErrInvalidSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return Session{}, ErrInvalidSession
	}
	sum := sha256.Sum256([]byte(token))
	var s Session
	var expires string
	err = db.QueryRowContext(ctx, `SELECT s.id,s.user_id,s.csrf_token,s.expires_at FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.revoked_at IS NULL AND u.status='active'`, hex.EncodeToString(sum[:])).Scan(&s.ID, &s.UserID, &s.CSRFToken, &expires)
	if err == sql.ErrNoRows {
		return Session{}, ErrInvalidSession
	}
	if err != nil {
		return Session{}, fmt.Errorf("lookup session: %w", err)
	}
	s.ExpiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil || !s.ExpiresAt.After(time.Now()) {
		return Session{}, ErrInvalidSession
	}
	s.Token = token
	return s, nil
}

// RevokeSession revokes the session identified by its bearer token.
func RevokeSession(ctx context.Context, db *sql.DB, token string) error {
	if len(token) != 43 {
		return ErrInvalidSession
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return ErrInvalidSession
	}
	sum := sha256.Sum256([]byte(token))
	r, err := db.ExecContext(ctx, `UPDATE sessions SET revoked_at=CURRENT_TIMESTAMP WHERE token_hash=? AND revoked_at IS NULL`, hex.EncodeToString(sum[:]))
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	if n != 1 {
		return ErrInvalidSession
	}
	return nil
}
