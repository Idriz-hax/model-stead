package server

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Idriz-hax/model-stead/internal/auth"
)

const sessionCookie = "__Host-afr_session"

var loginLimiter = struct {
	sync.Mutex
	attempts map[string][]time.Time
	global   []time.Time
}{attempts: map[string][]time.Time{}}

var argonWork = make(chan struct{}, 2)

func tryAcquireArgonWork() bool {
	select {
	case argonWork <- struct{}{}:
		return true
	default:
		return false
	}
}

func updatePasswordIfUnchanged(ctx context.Context, tx *sql.Tx, userID, oldHash, newHash string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE users SET password_hash=?,must_change_password=0,updated_at=CURRENT_TIMESTAMP WHERE id=? AND status='active' AND password_hash=?`, newHash, userID, oldHash)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func allowLogin(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if net.ParseIP(host) == nil {
		host = "unknown"
	}
	now := time.Now()
	loginLimiter.Lock()
	defer loginLimiter.Unlock()
	global := loginLimiter.global[:0]
	for _, t := range loginLimiter.global {
		if now.Sub(t) < time.Minute {
			global = append(global, t)
		}
	}
	loginLimiter.global = global
	if len(global) >= 1000 {
		return false
	}
	for k, ts := range loginLimiter.attempts {
		if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > time.Minute {
			delete(loginLimiter.attempts, k)
		}
	}
	if len(loginLimiter.attempts) >= 4096 && loginLimiter.attempts[host] == nil {
		return false
	}
	ts := loginLimiter.attempts[host]
	kept := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= 10 {
		loginLimiter.attempts[host] = kept
		return false
	}
	loginLimiter.attempts[host] = append(kept, now)
	loginLimiter.global = append(loginLimiter.global, now)
	return true
}

func registerAuth(m *http.ServeMux, db *sql.DB, cfg AuthConfig) {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	origin := func(r *http.Request) bool {
		return cfg.PublicOrigin != "" && r.Header.Get("Origin") == cfg.PublicOrigin
	}
	type user struct {
		ID                 string `json:"id"`
		Username           string `json:"username"`
		DisplayName        string `json:"display_name"`
		Role               string `json:"role"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	lookup := func(r *http.Request) (auth.Session, user, error) {
		c, e := r.Cookie(sessionCookie)
		if e != nil {
			return auth.Session{}, user{}, auth.ErrInvalidSession
		}
		s, e := auth.LookupSession(r.Context(), db, c.Value)
		if e != nil {
			return s, user{}, e
		}
		var u user
		var mustChange int
		e = db.QueryRowContext(r.Context(), `SELECT id,username,display_name,role,must_change_password FROM users WHERE id=? AND status='active'`, s.UserID).Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &mustChange)
		u.MustChangePassword = mustChange != 0
		if e == sql.ErrNoRows {
			return auth.Session{}, user{}, auth.ErrInvalidSession
		}
		if e != nil {
			return auth.Session{}, user{}, fmt.Errorf("load session user: %w", e)
		}
		return s, u, e
	}
	sessionFailure := func(w http.ResponseWriter, err error) {
		if errors.Is(err, auth.ErrInvalidSession) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}
	unauth := func(w http.ResponseWriter) { http.Error(w, "unauthorized", http.StatusUnauthorized) }
	m.HandleFunc("/api/v1/auth/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	m.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !origin(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		if !allowLogin(r.RemoteAddr) {
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		}
		var in struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if !tryAcquireArgonWork() {
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		}
		defer func() { <-argonWork }()
		var id, hash string
		var mustChange int
		e := db.QueryRowContext(r.Context(), `SELECT id,password_hash,must_change_password FROM users WHERE username=? AND status='active'`, in.Username).Scan(&id, &hash, &mustChange)
		valid := false
		if e == nil {
			var verifyErr error
			valid, verifyErr = auth.VerifyPassword(hash, in.Password)
			if verifyErr != nil {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
		} else if e != sql.ErrNoRows {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !valid {
			unauth(w)
			return
		}
		s, e := auth.CreateSession(r.Context(), db, id, cfg.SessionTTL)
		if e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: s.Token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: s.ExpiresAt})
		write(w, map[string]any{"csrf_token": s.CSRFToken, "must_change_password": mustChange != 0})
	})
	m.HandleFunc("GET /api/v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		s, u, e := lookup(r)
		if e != nil {
			sessionFailure(w, e)
			return
		}
		write(w, map[string]any{"csrf_token": s.CSRFToken, "user": u})
	})
	m.HandleFunc("POST /api/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		s, _, e := lookup(r)
		if e != nil {
			sessionFailure(w, e)
			return
		}
		if !origin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRFToken)) != 1 {
			http.Error(w, "forbidden", 403)
			return
		}
		if err := auth.RevokeSession(r.Context(), db, s.Token); err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
		write(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("POST /api/v1/auth/change-password", func(w http.ResponseWriter, r *http.Request) {
		s, u, e := lookup(r)
		if e != nil {
			sessionFailure(w, e)
			return
		}
		if !origin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRFToken)) != 1 {
			http.Error(w, "forbidden", 403)
			return
		}
		var in struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || len(in.NewPassword) < 12 {
			http.Error(w, "bad request", 400)
			return
		}
		var old string
		if err := db.QueryRowContext(r.Context(), `SELECT password_hash FROM users WHERE id=? AND status='active'`, s.UserID).Scan(&old); err != nil {
			if err == sql.ErrNoRows {
				unauth(w)
			} else {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if !tryAcquireArgonWork() {
			http.Error(w, "too many attempts", http.StatusTooManyRequests)
			return
		}
		defer func() { <-argonWork }()
		ok, err := auth.VerifyPassword(old, in.OldPassword)
		if err != nil {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if !ok {
			unauth(w)
			return
		}
		hash, e := auth.HashPassword(in.NewPassword)
		if e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		tx, e := db.BeginTx(r.Context(), nil)
		stalePassword := false
		if e == nil {
			var updated bool
			updated, e = updatePasswordIfUnchanged(r.Context(), tx, s.UserID, old, hash)
			if e == nil && !updated {
				stalePassword = true
				_ = tx.Rollback()
				e = sql.ErrNoRows
			} else if e == nil {
				_, e = tx.ExecContext(r.Context(), `UPDATE sessions SET revoked_at=CURRENT_TIMESTAMP WHERE user_id=? AND revoked_at IS NULL`, s.UserID)
			}
			if e != nil {
				if !stalePassword {
					_ = tx.Rollback()
				}
			} else {
				e = tx.Commit()
			}
		}
		if stalePassword {
			unauth(w)
			return
		}
		if e != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
		write(w, map[string]any{"ok": true, "user": map[string]string{"role": u.Role, "status": "active"}, "must_change_password": false})
	})
}
