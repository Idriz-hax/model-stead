package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Idriz-hax/model-stead/internal/admin"
)

var ErrSetupClosed = errors.New("setup closed")
var ErrSetupCode = errors.New("invalid setup code")

type SetupManager struct {
	db       *sql.DB
	mu       sync.Mutex
	active   bool
	codeHash [32]byte
}

func NewSetupManager(ctx context.Context, db *sql.DB) (*SetupManager, string, error) {
	m := &SetupManager{db: db}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		return nil, "", err
	}
	if n > 0 {
		return m, "", nil
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, "", err
	}
	code := base64.RawURLEncoding.EncodeToString(b[:])
	m.codeHash, m.active = sha256.Sum256([]byte(code)), true
	return m, code, nil
}

func (s *SetupManager) Required() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.active }

func (s *SetupManager) Claim(ctx context.Context, code string, user admin.User, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return ErrSetupClosed
	}
	h := sha256.Sum256([]byte(code))
	if subtle.ConstantTimeCompare(h[:], s.codeHash[:]) != 1 {
		return ErrSetupCode
	}
	var existing int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		s.active = false
		s.codeHash = [32]byte{}
		return ErrSetupClosed
	}
	if _, err := (admin.Store{DB: s.db}).CreateFirstAdmin(ctx, user, password); err != nil {
		var count int
		if s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&count) == nil && count > 0 {
			s.active = false
			s.codeHash = [32]byte{}
			return ErrSetupClosed
		}
		return err
	}
	s.active = false
	s.codeHash = [32]byte{}
	return nil
}

func registerSetup(m *http.ServeMux, db *sql.DB, cfg AuthConfig) {
	manager := cfg.Setup
	if manager == nil {
		manager = &SetupManager{db: db}
	}
	registerSetupManager(m, manager, cfg)
}

func registerSetupManager(m *http.ServeMux, manager *SetupManager, cfg AuthConfig) {
	origin := func(r *http.Request) bool {
		return cfg.PublicOrigin != "" && r.Header.Get("Origin") == cfg.PublicOrigin
	}
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	m.HandleFunc("GET /api/v1/auth/setup-state", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]bool{"setup_required": manager.Required()})
	})
	m.HandleFunc("POST /api/v1/auth/setup", func(w http.ResponseWriter, r *http.Request) {
		if !origin(r) {
			http.Error(w, "forbidden", 403)
			return
		}
		if !allowLogin(r.RemoteAddr) {
			http.Error(w, "too many attempts", 429)
			return
		}
		var in struct {
			Code         string `json:"code"`
			Username     string `json:"username"`
			DisplayName  string `json:"display_name"`
			Password     string `json:"password"`
			Confirmation string `json:"password_confirmation"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF || in.Password != in.Confirmation || len(in.Password) < 12 || len(in.Password) > 1024 {
			http.Error(w, "bad request", 400)
			return
		}
		in.Username = strings.TrimSpace(in.Username)
		if admin.ValidateUsername(in.Username) != nil || admin.ValidateDisplayName(in.DisplayName) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if !tryAcquireArgonWork() {
			http.Error(w, "too many attempts", 429)
			return
		}
		defer func() { <-argonWork }()
		err := manager.Claim(r.Context(), in.Code, admin.User{Username: in.Username, DisplayName: in.DisplayName, Role: "admin", SchedulingWeight: 1, MaxQueued: 100, MaxActiveContended: 1}, in.Password)
		switch {
		case errors.Is(err, ErrSetupClosed):
			http.Error(w, "setup closed", 409)
		case errors.Is(err, ErrSetupCode):
			http.Error(w, "unauthorized", 401)
		case err != nil:
			http.Error(w, "unavailable", 503)
		default:
			w.WriteHeader(http.StatusCreated)
			write(w, map[string]bool{"ok": true})
		}
	})
}
