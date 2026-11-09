package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AuthErrorKind string

const (
	Unauthorized AuthErrorKind = "unauthorized"
	Unavailable  AuthErrorKind = "unavailable"
	Capacity     AuthErrorKind = "capacity"
)

type AuthError struct {
	Kind AuthErrorKind
	Err  error
}

func (e *AuthError) Error() string {
	if e.Err != nil {
		return string(e.Kind) + ": " + e.Err.Error()
	}
	return string(e.Kind)
}
func (e *AuthError) Unwrap() error { return e.Err }
func IsUnauthorized(err error) bool {
	var e *AuthError
	return errors.As(err, &e) && e.Kind == Unauthorized
}
func IsCapacity(err error) bool {
	var e *AuthError
	return errors.As(err, &e) && e.Kind == Capacity
}
func unavailable(err error) error { return &AuthError{Kind: Unavailable, Err: err} }
func unauthorized() error         { return &AuthError{Kind: Unauthorized} }

type Identity struct {
	SchedulingID string
	UserID       string
	Role         string
	AnonymousID  string
	AuthMethod   string
}

type identityContextKey struct{}

// WithIdentity adds a resolved identity to a request context for downstream services.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

// IdentityFromContext returns the resolved identity carried by ctx, if any.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(Identity)
	return identity, ok
}

type ResolverConfig struct {
	DisableAPIKeys         bool
	IPFallbackEnabled      bool
	TrustedProxyPrefixes   []netip.Prefix
	AnonymousEnabled       bool
	AllowedAnonymousCIDRs  []netip.Prefix
	AnonymousSecret        []byte
	MaxAnonymousIdentities int64
}
type Resolver struct {
	db  *sql.DB
	cfg ResolverConfig
}

func NewResolver(db *sql.DB, cfg ResolverConfig) *Resolver { return &Resolver{db: db, cfg: cfg} }

func CreateAPIKey(ctx context.Context, db *sql.DB, userID, name string) (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", unavailable(err)
	}
	secret := "afr_v1_" + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	id := uuid.NewString()
	result, err := db.ExecContext(ctx, `INSERT INTO api_keys(id,user_id,name,key_prefix,key_hash,status) SELECT ?,?,?,?,?,'active' WHERE EXISTS (SELECT 1 FROM users WHERE id=? AND status='active')`, id, userID, name, secret[:14], hex.EncodeToString(sum[:]), userID)
	if err != nil {
		return "", "", unavailable(err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return "", "", unavailable(err)
	}
	if rows != 1 {
		return "", "", unauthorized()
	}
	return secret, id, nil
}

func (r *Resolver) Resolve(ctx context.Context, headers http.Header, remoteAddr string) (Identity, error) {
	authVals, authPresent := credentialHeaderValues(headers, "Authorization")
	keyVals, keyPresent := credentialHeaderValues(headers, "X-API-Key")
	credential := authPresent || keyPresent
	if credential && r.cfg.DisableAPIKeys {
		return Identity{}, unauthorized()
	}
	var keys []string
	if authPresent && len(authVals) != 1 || keyPresent && len(keyVals) != 1 {
		return Identity{}, unauthorized()
	}
	if len(authVals) == 1 {
		p := strings.Fields(authVals[0])
		if len(p) != 2 || !strings.EqualFold(p[0], "Bearer") || p[1] == "" {
			return Identity{}, unauthorized()
		}
		keys = append(keys, p[1])
	}
	if len(keyVals) == 1 {
		if strings.TrimSpace(keyVals[0]) == "" || strings.TrimSpace(keyVals[0]) != keyVals[0] {
			return Identity{}, unauthorized()
		}
		keys = append(keys, keyVals[0])
	}
	if credential {
		var identity Identity
		for i, k := range keys {
			got, err := r.resolveKey(ctx, k)
			if err != nil {
				return Identity{}, err
			}
			if i > 0 && got.UserID != identity.UserID {
				return Identity{}, unauthorized()
			}
			identity = got
		}
		return identity, nil
	}
	peer, err := parseAddr(remoteAddr)
	if err != nil {
		return Identity{}, unauthorized()
	}
	client := peer
	if contains(r.cfg.TrustedProxyPrefixes, peer) {
		client, err = forwarded(headers, peer, r.cfg.TrustedProxyPrefixes)
		if err != nil {
			return Identity{}, unauthorized()
		}
	}
	if r.cfg.IPFallbackEnabled {
		var id, user, role string
		err = r.db.QueryRowContext(ctx, `SELECT d.id,u.id,u.role FROM devices d JOIN users u ON u.id=d.user_id WHERE d.ip_address=? AND d.trusted=1 AND u.status='active'`, client.String()).Scan(&id, &user, &role)
		if err == nil {
			_, e := r.db.ExecContext(ctx, `UPDATE devices SET last_seen_at=CURRENT_TIMESTAMP WHERE id=?`, id)
			if e != nil {
				return Identity{}, unavailable(e)
			}
			return Identity{SchedulingID: user, UserID: user, Role: role, AuthMethod: "device"}, nil
		}
		if err != sql.ErrNoRows {
			return Identity{}, unavailable(err)
		}
	}
	if !r.cfg.AnonymousEnabled || len(r.cfg.AnonymousSecret) == 0 || !contains(r.cfg.AllowedAnonymousCIDRs, client) {
		return Identity{}, unauthorized()
	}
	maxAnonymous := r.cfg.MaxAnonymousIdentities
	if maxAnonymous <= 0 {
		maxAnonymous = 100_000
	}
	mac := hmac.New(sha256.New, r.cfg.AnonymousSecret)
	mac.Write([]byte("anonymous-ip:v1:" + client.String()))
	fingerprint := hex.EncodeToString(mac.Sum(nil))
	anonID := uuid.NewString()
	result, err := r.db.ExecContext(ctx, `INSERT INTO anonymous_identities(id,fingerprint) SELECT ?,? WHERE EXISTS (SELECT 1 FROM anonymous_identities WHERE fingerprint=?) OR (SELECT COUNT(*) FROM anonymous_identities) < ? ON CONFLICT(fingerprint) DO UPDATE SET last_seen_at=CURRENT_TIMESTAMP`, anonID, fingerprint, fingerprint, maxAnonymous)
	if err != nil {
		return Identity{}, unavailable(err)
	}
	if inserted, e := result.RowsAffected(); e != nil {
		return Identity{}, unavailable(e)
	} else if inserted == 0 {
		return Identity{}, &AuthError{Kind: Capacity}
	}
	if err = r.db.QueryRowContext(ctx, `SELECT id FROM anonymous_identities WHERE fingerprint=?`, fingerprint).Scan(&anonID); err != nil {
		return Identity{}, unavailable(err)
	}
	return Identity{SchedulingID: anonID, AnonymousID: anonID, AuthMethod: "anonymous"}, nil
}

func credentialHeaderValues(h http.Header, name string) ([]string, bool) {
	var values []string
	present := false
	for key, entries := range h {
		if strings.EqualFold(key, name) {
			present = true
			values = append(values, entries...)
		}
	}
	return values, present
}

func (r *Resolver) resolveKey(ctx context.Context, key string) (Identity, error) {
	if !strings.HasPrefix(key, "afr_v1_") || len(key) != 50 {
		return Identity{}, unauthorized()
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, "afr_v1_"))
	if err != nil || len(raw) != 32 {
		return Identity{}, unauthorized()
	}
	sum := sha256.Sum256([]byte(key))
	var id, user, role, status string
	var expires sql.NullString
	err = r.db.QueryRowContext(ctx, `SELECT k.id,u.id,u.role,u.status,k.expires_at FROM api_keys k JOIN users u ON u.id=k.user_id WHERE k.key_hash=? AND k.status='active'`, hex.EncodeToString(sum[:])).Scan(&id, &user, &role, &status, &expires)
	if err == sql.ErrNoRows {
		return Identity{}, unauthorized()
	}
	if err != nil {
		return Identity{}, unavailable(err)
	}
	if status != "active" || expires.Valid && expired(expires.String) {
		return Identity{}, unauthorized()
	}
	if _, err = r.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=CURRENT_TIMESTAMP WHERE id=?`, id); err != nil {
		return Identity{}, unavailable(err)
	}
	return Identity{SchedulingID: user, UserID: user, Role: role, AuthMethod: "api_key"}, nil
}
func expired(s string) bool {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05"} {
		if t, e := time.Parse(layout, s); e == nil {
			return !t.After(time.Now())
		}
	}
	return true
}
func parseAddr(s string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	a, e := netip.ParseAddr(host)
	if e != nil {
		return netip.Addr{}, e
	}
	return a.Unmap(), nil
}
func contains(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.IsValid() && p.Contains(a) {
			return true
		}
	}
	return false
}
func forwarded(h http.Header, peer netip.Addr, trusted []netip.Prefix) (netip.Addr, error) {
	v, present := credentialHeaderValues(h, "X-Forwarded-For")
	if !present || len(v) != 1 || len(v[0]) > 2048 {
		return netip.Addr{}, fmt.Errorf("invalid forwarding header")
	}
	raw := strings.Split(v[0], ",")
	if len(raw) > 20 {
		return netip.Addr{}, fmt.Errorf("too many hops")
	}
	addrs := make([]netip.Addr, 0, len(raw))
	for _, s := range raw {
		a, e := parseAddr(strings.TrimSpace(s))
		if e != nil {
			return netip.Addr{}, e
		}
		addrs = append(addrs, a)
	}
	cur := peer
	for i := len(addrs) - 1; i >= 0 && contains(trusted, cur); i-- {
		cur = addrs[i]
	}
	return cur, nil
}
