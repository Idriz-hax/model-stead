package auth

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/Idriz-hax/model-stead/internal/database"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "identity.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('u','user','User','user','active'),('disabled','disabled','Disabled','user','disabled')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
func header(name, value string) http.Header { h := make(http.Header); h.Set(name, value); return h }
func expectUnauthorized(t *testing.T, err error) {
	t.Helper()
	if !IsUnauthorized(err) {
		t.Fatalf("expected unauthorized, got %v", err)
	}
}
func makeKey(t *testing.T, db *sql.DB) string {
	t.Helper()
	key, _, err := CreateAPIKey(context.Background(), db, "u", "test")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestResolveMultipleKeysSameUser(t *testing.T) {
	db := testDB(t)
	a, b := makeKey(t, db), makeKey(t, db)
	h := header("Authorization", "Bearer "+a)
	h.Set("X-API-Key", b)
	got, err := NewResolver(db, ResolverConfig{}).Resolve(context.Background(), h, "192.0.2.1:1")
	if err != nil || got.UserID != "u" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestAPICanDisableAPIKeyResolution(t *testing.T) {
	db := testDB(t)
	key := makeKey(t, db)
	expectUnauthorized(t, resolve(t, db, header("X-API-Key", key), "192.0.2.1:80", ResolverConfig{DisableAPIKeys: true}))
}

func TestIdentityContextRoundTrip(t *testing.T) {
	want := Identity{SchedulingID: "u", UserID: "u", Role: "user", AuthMethod: "api_key"}
	got, ok := IdentityFromContext(WithIdentity(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("identity=%+v found=%v", got, ok)
	}
}
func TestInvalidCredentialCases(t *testing.T) {
	db := testDB(t)
	key := makeKey(t, db)
	if _, _, err := CreateAPIKey(context.Background(), db, "disabled", "x"); !IsUnauthorized(err) {
		t.Fatalf("disabled create: %v", err)
	}
	for _, tc := range []struct {
		name, token string
		change      func(*testing.T)
	}{
		{"unknown", "afr_v1_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil},
		{"revoked", key, func(t *testing.T) {
			if _, e := db.Exec(`UPDATE api_keys SET status='revoked'`); e != nil {
				t.Fatal(e)
			}
		}},
		{"expired", key, func(t *testing.T) {
			if _, e := db.Exec(`UPDATE api_keys SET status='active',expires_at='2000-01-01 00:00:00'`); e != nil {
				t.Fatal(e)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.change != nil {
				tc.change(t)
			}
			expectUnauthorized(t, resolve(t, db, header("X-API-Key", tc.token), "198.51.100.2:80", ResolverConfig{}))
		})
	}
}
func TestCredentialHeadersFailClosed(t *testing.T) {
	db := testDB(t)
	a, b := makeKey(t, db), makeKey(t, db)
	cases := []http.Header{
		{"X-API-Key": {"", a}},
		{"Authorization": {"Basic " + a}},
		{"Authorization": {"Bearer "}},
		{"Authorization": {"Bearer " + a}, "X-API-Key": {"bad"}},
		{"authorization": {"Bearer " + a}, "Authorization": {"Bearer " + a}},
	}
	got, err := NewResolver(db, ResolverConfig{}).Resolve(context.Background(), http.Header{"Authorization": {"Bearer " + a}, "X-API-Key": {b}}, "192.0.2.1:80")
	if err != nil || got.UserID != "u" {
		t.Fatalf("same-user keys rejected: identity=%+v err=%v", got, err)
	}
	for i, h := range cases {
		t.Run(string(rune('a'+i)), func(t *testing.T) { expectUnauthorized(t, resolve(t, db, h, "192.0.2.1:80", ResolverConfig{})) })
	}
}
func TestDeviceFallbackGateAndCredentialNoFallback(t *testing.T) {
	db := testDB(t)
	_, err := db.Exec(`INSERT INTO devices(id,user_id,label,ip_address,trusted) VALUES('d','u','device','192.0.2.1',1)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true} {
		got, e := NewResolver(db, ResolverConfig{IPFallbackEnabled: enabled}).Resolve(context.Background(), nil, "192.0.2.1:80")
		if enabled && (e != nil || got.UserID != "u") {
			t.Fatalf("enabled identity=%+v err=%v", got, e)
		}
		if !enabled {
			expectUnauthorized(t, e)
		}
	}
	expectUnauthorized(t, resolve(t, db, header("X-API-Key", "invalid"), "192.0.2.1:80", ResolverConfig{IPFallbackEnabled: true}))
}
func TestForwardedForTrustAndHops(t *testing.T) {
	db := testDB(t)
	_, err := db.Exec(`INSERT INTO devices(id,user_id,label,ip_address,trusted) VALUES('d','u','device','198.51.100.7',1)`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ResolverConfig{IPFallbackEnabled: true, TrustedProxyPrefixes: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	h := header("X-Forwarded-For", "198.51.100.7")
	got, e := NewResolver(db, cfg).Resolve(context.Background(), h, "203.0.113.1:80")
	expectUnauthorized(t, e)
	_ = got
	h.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.2, 10.0.0.3")
	got, e = NewResolver(db, cfg).Resolve(context.Background(), h, "10.0.0.1:80")
	if e != nil || got.UserID != "u" {
		t.Fatalf("identity=%+v err=%v", got, e)
	}
}
func TestAnonymousIdentityAllowedBoundaryAndStable(t *testing.T) {
	db := testDB(t)
	cfg := ResolverConfig{AnonymousEnabled: true, AnonymousSecret: []byte("persistent secret"), AllowedAnonymousCIDRs: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	r := NewResolver(db, cfg)
	a, e := r.Resolve(context.Background(), nil, "192.0.2.0:1")
	if e != nil {
		t.Fatal(e)
	}
	b, e := r.Resolve(context.Background(), nil, "192.0.2.0:2")
	if e != nil || a.AnonymousID != b.AnonymousID {
		t.Fatalf("a=%+v b=%+v err=%v", a, b, e)
	}
	expectUnauthorized(t, resolve(t, db, nil, "192.0.3.1:1", cfg))
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM anonymous_identities`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestAnonymousIdentitiesRespectCapacity(t *testing.T) {
	db := testDB(t)
	cfg := ResolverConfig{
		AnonymousEnabled:       true,
		AnonymousSecret:        []byte("persistent secret at least 32 bytes long"),
		AllowedAnonymousCIDRs:  []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")},
		MaxAnonymousIdentities: 1,
	}
	r := NewResolver(db, cfg)
	if _, err := r.Resolve(context.Background(), nil, "192.0.2.1:80"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(context.Background(), nil, "192.0.2.2:80"); !IsCapacity(err) {
		t.Fatalf("expected capacity error, got %v", err)
	}
	if _, err := r.Resolve(context.Background(), nil, "192.0.2.1:80"); err != nil {
		t.Fatalf("existing identity should remain usable: %v", err)
	}
}
func TestUnavailableOnDatabaseFailure(t *testing.T) {
	db := testDB(t)
	_ = db.Close()
	_, err := NewResolver(db, ResolverConfig{}).Resolve(context.Background(), header("X-API-Key", "afr_v1_"+"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"), "192.0.2.1:1")
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Kind != Unavailable {
		t.Fatalf("expected unavailable, got %v", err)
	}
}
func resolve(t *testing.T, db *sql.DB, h http.Header, addr string, cfg ResolverConfig) error {
	t.Helper()
	_, err := NewResolver(db, cfg).Resolve(context.Background(), h, addr)
	return err
}
