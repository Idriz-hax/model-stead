package server

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"

	"github.com/Idriz-hax/model-stead/internal/auth"
)

type sessionPrincipal struct {
	session      auth.Session
	userID, role string
	forced       bool
}

func lookupSessionPrincipal(r *http.Request, db *sql.DB) (sessionPrincipal, int) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return sessionPrincipal{}, http.StatusUnauthorized
	}
	s, err := auth.LookupSession(r.Context(), db, c.Value)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidSession) {
			return sessionPrincipal{}, http.StatusUnauthorized
		}
		return sessionPrincipal{}, http.StatusServiceUnavailable
	}
	p := sessionPrincipal{session: s, userID: s.UserID}
	var forced int
	if err := db.QueryRowContext(r.Context(), `SELECT role,must_change_password FROM users WHERE id=? AND status='active'`, s.UserID).Scan(&p.role, &forced); err != nil {
		if err == sql.ErrNoRows {
			return sessionPrincipal{}, http.StatusUnauthorized
		}
		return sessionPrincipal{}, http.StatusServiceUnavailable
	}
	p.forced = forced != 0
	if p.forced {
		return sessionPrincipal{}, http.StatusForbidden
	}
	return p, 0
}

func sessionCSRFValid(r *http.Request, ac AuthConfig, p sessionPrincipal) bool {
	return ac.PublicOrigin != "" && r.Header.Get("Origin") == ac.PublicOrigin && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(p.session.CSRFToken)) == 1
}
