package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Idriz-hax/model-stead/internal/admin"
	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/Idriz-hax/model-stead/internal/database"
)

func TestAuthEndpoints(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const oldPassword = "correct horse battery staple"
	hash, err := auth.HashPassword(oldPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO users(id,username,display_name,password_hash,role,status) VALUES('u1','alice','Alice',?,'admin','active')`, hash); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO api_keys(id,user_id,name,key_prefix,key_hash,status) VALUES('k1','u1','key','prefix','hash','active')`); err != nil {
		t.Fatal(err)
	}
	h := NewWithConfig(testFiles, db, GatewayConfig{}, AuthConfig{PublicOrigin: "https://router.example"})
	request := func(method, path, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	login := func(body string) *httptest.ResponseRecorder {
		return request("POST", "/api/v1/auth/login", body, nil, "", "https://router.example")
	}

	bad := login(`{"username":"alice","password":"wrong"}`)
	if bad.Code != http.StatusUnauthorized || strings.Contains(bad.Body.String(), "wrong") {
		t.Fatalf("invalid login: %d %s", bad.Code, bad.Body.String())
	}
	// Credentials are the only accepted login mechanism; an API key is not a dashboard session.
	apiKey := login(`{"api_key":"hash"}`)
	if apiKey.Code != http.StatusUnauthorized || len(apiKey.Result().Cookies()) != 0 {
		t.Fatalf("API key login: %d", apiKey.Code)
	}

	w := login(`{"username":"alice","password":"` + oldPassword + `"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies: %v", cookies)
	}
	c := cookies[0]
	if c.Name != "__Host-afr_session" || c.Path != "/" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Value == "" {
		t.Fatalf("session cookie: %+v", c)
	}
	var loginBody map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &loginBody); err != nil || len(loginBody["csrf_token"]) == 0 {
		t.Fatalf("login CSRF response: %s (%v)", w.Body.String(), err)
	}
	var csrf string
	_ = json.Unmarshal(loginBody["csrf_token"], &csrf)
	if got := request("GET", "/api/v1/auth/me", "", nil, "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("me without cookie: %d", got.Code)
	}
	me := request("GET", "/api/v1/auth/me", "", c, "", "")
	var meBody map[string]json.RawMessage
	if me.Code != http.StatusOK || json.Unmarshal(me.Body.Bytes(), &meBody) != nil || len(meBody["csrf_token"]) == 0 {
		t.Fatalf("me: %d %s", me.Code, me.Body.String())
	}
	if got := request("POST", "/api/v1/auth/logout", "", c, "", "https://router.example"); got.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF: %d", got.Code)
	}
	logout := request("POST", "/api/v1/auth/logout", "", c, csrf, "https://router.example")
	if logout.Code != http.StatusOK || logout.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("logout: %d cookies=%v", logout.Code, logout.Result().Cookies())
	}
	if got := request("GET", "/api/v1/auth/me", "", c, "", ""); got.Code != http.StatusUnauthorized {
		t.Fatalf("logout did not revoke session: %d", got.Code)
	}

	// A second active session must also be revoked when the password changes.
	if _, err := db.Exec(`UPDATE users SET must_change_password=1 WHERE id='u1'`); err != nil {
		t.Fatal(err)
	}
	w = login(`{"username":"alice","password":"` + oldPassword + `"}`)
	c = w.Result().Cookies()[0]
	json.Unmarshal(w.Body.Bytes(), &loginBody)
	_ = json.Unmarshal(loginBody["csrf_token"], &csrf)
	var forced bool
	if json.Unmarshal(loginBody["must_change_password"], &forced) != nil || !forced {
		t.Fatalf("login omitted forced-change flag: %s", w.Body.String())
	}
	me = request("GET", "/api/v1/auth/me", "", c, "", "")
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"must_change_password":true`) || !strings.Contains(me.Body.String(), `"display_name":"Alice"`) || !strings.Contains(me.Body.String(), `"username":"alice"`) || !strings.Contains(me.Body.String(), `"id":"u1"`) {
		t.Fatalf("me omitted forced-change flag: %d %s", me.Code, me.Body.String())
	}
	body := `{"old_password":"` + oldPassword + `","new_password":"new sufficiently long password"}`
	if got := request("POST", "/api/v1/auth/change-password", body, c, csrf, "https://evil.example"); got.Code != http.StatusForbidden {
		t.Fatalf("cross-origin change: %d", got.Code)
	}
	if got := request("POST", "/api/v1/auth/change-password", body, c, "bad", "https://router.example"); got.Code != http.StatusForbidden {
		t.Fatalf("CSRF change: %d", got.Code)
	}
	other := login(`{"username":"alice","password":"` + oldPassword + `"}`)
	if other.Code != http.StatusOK {
		t.Fatalf("second login: %d", other.Code)
	}
	changed := request("POST", "/api/v1/auth/change-password", body, c, csrf, "https://router.example")
	if changed.Code != http.StatusOK || changed.Result().Cookies()[0].MaxAge >= 0 {
		t.Fatalf("change password: %d", changed.Code)
	}
	if !strings.Contains(changed.Body.String(), `"role":"admin"`) || !strings.Contains(changed.Body.String(), `"must_change_password":false`) {
		t.Fatalf("change response missing safe onboarding result: %s", changed.Body.String())
	}
	var must int
	if err := db.QueryRow(`SELECT must_change_password FROM users WHERE id='u1'`).Scan(&must); err != nil || must != 0 {
		t.Fatalf("forced flag not cleared: %d %v", must, err)
	}
	for _, stale := range []*http.Cookie{c, other.Result().Cookies()[0]} {
		if got := request("GET", "/api/v1/auth/me", "", stale, "", ""); got.Code != http.StatusUnauthorized {
			t.Fatalf("session survived password change: %d", got.Code)
		}
	}
	if got := login(`{"username":"alice","password":"` + oldPassword + `"}`); got.Code != http.StatusUnauthorized {
		t.Fatalf("old password accepted: %d", got.Code)
	}
	if got := login(`{"username":"alice","password":"new sufficiently long password"}`); got.Code != http.StatusOK {
		t.Fatalf("new password rejected: %d", got.Code)
	}
}

func TestArgonWorkSlotsBoundConcurrentPasswordOperations(t *testing.T) {
	for range cap(argonWork) {
		if !tryAcquireArgonWork() {
			t.Fatal("failed to acquire available Argon2 slot")
		}
	}
	if tryAcquireArgonWork() {
		<-argonWork
		t.Fatal("acquired Argon2 slot beyond the concurrency limit")
	}
	<-argonWork
	if !tryAcquireArgonWork() {
		t.Fatal("slot was not released")
	}
	for range cap(argonWork) {
		<-argonWork
	}
}

func TestFirstAdminSetup(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/setup.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO scheduler_settings(id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES(1,'weighted_round_robin',4,1,10,2,20,60,1,5)`); err != nil {
		t.Fatal(err)
	}
	manager, code, err := NewSetupManager(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithConfig(testFiles, db, GatewayConfig{}, AuthConfig{PublicOrigin: "https://router.example", Setup: manager})
	state := httptest.NewRecorder()
	h.ServeHTTP(state, httptest.NewRequest("GET", "/api/v1/auth/setup-state", nil))
	if state.Code != 200 || state.Body.String() != "{\"setup_required\":true}\n" {
		t.Fatalf("state: %d %s", state.Code, state.Body.String())
	}
	body := `{"code":"` + code + `","username":"owner","display_name":"Owner","password":"long enough secure password","password_confirmation":"long enough secure password"}`
	requestN := 0
	req := func(origin string) *httptest.ResponseRecorder {
		requestN++
		r := httptest.NewRequest("POST", "/api/v1/auth/setup", strings.NewReader(body))
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestN)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	badBody := strings.Replace(body, code, "wrong", 1)
	badReq := httptest.NewRequest("POST", "/api/v1/auth/setup", strings.NewReader(badBody))
	requestN++
	badReq.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestN)
	badReq.Header.Set("Origin", "https://router.example")
	badRes := httptest.NewRecorder()
	h.ServeHTTP(badRes, badReq)
	if badRes.Code != 401 || strings.Contains(badRes.Body.String(), code) || strings.Contains(badRes.Body.String(), "password") {
		t.Fatalf("bad code: %d %s", badRes.Code, badRes.Body.String())
	}
	if got := req("https://evil.example"); got.Code != 403 {
		t.Fatalf("wrong origin: %d", got.Code)
	}
	if got := req("https://router.example"); got.Code != 201 {
		t.Fatalf("create: %d %s", got.Code, got.Body.String())
	}
	if got := req("https://router.example"); got.Code != 409 {
		t.Fatalf("repeat: %d", got.Code)
	}
	for _, invalid := range []string{strings.Repeat("x", 4097), `{"code":"x","username":"owner","display_name":"Owner","password":"short","password_confirmation":"short"}`, `{"code":"x","username":"owner","display_name":"Owner","password":"long enough secure password","password_confirmation":"not matching password"}`} {
		r := httptest.NewRequest("POST", "/api/v1/auth/setup", strings.NewReader(invalid))
		requestN++
		r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestN)
		r.Header.Set("Origin", "https://router.example")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid request status %d", w.Code)
		}
	}
	rotationDB, err := database.Open(t.TempDir() + "/rotation.db")
	if err != nil {
		t.Fatal(err)
	}
	defer rotationDB.Close()
	one, first, err := NewSetupManager(context.Background(), rotationDB)
	if err != nil {
		t.Fatal(err)
	}
	two, second, err := NewSetupManager(context.Background(), rotationDB)
	if err != nil || !one.Required() || !two.Required() || first == second {
		t.Fatalf("rotation: %v %v %v", one.Required(), two.Required(), err)
	}
	if err := two.Claim(context.Background(), first, admin.User{Username: "owner", DisplayName: "Owner", Role: "admin", SchedulingWeight: 1, MaxQueued: 10, MaxActiveContended: 1}, "long enough password"); !errors.Is(err, ErrSetupCode) || !two.Required() {
		t.Fatalf("old code claim: err=%v required=%v", err, two.Required())
	}
}

func TestFirstAdminSetupExistingDisabledAdmin(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/disabled.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO users(id,username,display_name,password_hash,role,status) VALUES('disabled','owner','Owner','x','admin','disabled')`); err != nil {
		t.Fatal(err)
	}
	manager, code, err := NewSetupManager(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if code != "" || manager.Required() {
		t.Fatalf("existing disabled admin opened setup: code=%q required=%v", code, manager.Required())
	}
	h := NewWithConfig(testFiles, db, GatewayConfig{}, AuthConfig{PublicOrigin: "https://router.example", Setup: manager})
	state := httptest.NewRecorder()
	h.ServeHTTP(state, httptest.NewRequest("GET", "/api/v1/auth/setup-state", nil))
	if state.Code != 200 || state.Body.String() != "{\"setup_required\":false}\n" {
		t.Fatalf("state: %d %s", state.Code, state.Body.String())
	}
	r := httptest.NewRequest("POST", "/api/v1/auth/setup", strings.NewReader(`{"code":"","username":"second","display_name":"Second","password":"long enough password","password_confirmation":"long enough password"}`))
	r.Header.Set("Origin", "https://router.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatalf("setup POST: %d %s", w.Code, w.Body.String())
	}
	if err := manager.Claim(context.Background(), "", admin.User{Username: "second", DisplayName: "Second", Role: "admin", SchedulingWeight: 1}, "long enough password"); !errors.Is(err, ErrSetupClosed) {
		t.Fatalf("Claim: %v", err)
	}
}

func TestFirstAdminSetupStaleManager(t *testing.T) {
	db, err := database.Open(t.TempDir() + "/stale.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO scheduler_settings(id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES(1,'weighted_round_robin',4,1,10,2,20,60,1,5)`); err != nil {
		t.Fatal(err)
	}
	manager, code, err := NewSetupManager(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = (admin.Store{DB: db}).CreateFirstAdmin(context.Background(), admin.User{Username: "first", DisplayName: "First", Role: "admin", SchedulingWeight: 1, MaxQueued: 10, MaxActiveContended: 1}, "long enough password"); err != nil {
		t.Fatal(err)
	}
	err = manager.Claim(context.Background(), code, admin.User{Username: "second", DisplayName: "Second", Role: "admin", SchedulingWeight: 1, MaxQueued: 10, MaxActiveContended: 1}, "another long password")
	if !errors.Is(err, ErrSetupClosed) || manager.Required() {
		t.Fatalf("stale claim err=%v required=%v", err, manager.Required())
	}
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("admin count=%d err=%v", n, err)
	}
}

func TestFirstAdminSetupConcurrentManagers(t *testing.T) {
	path := t.TempDir() + "/concurrent.db"
	db1, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	if _, err = db1.Exec(`INSERT INTO scheduler_settings(id,policy,global_capacity,default_weight,max_queued,max_active_contended,global_max_queued,max_wait_seconds,work_conserving,cancellation_grace_seconds) VALUES(1,'weighted_round_robin',4,1,10,2,20,60,1,5)`); err != nil {
		t.Fatal(err)
	}
	db2, err := database.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	m1, c1, err := NewSetupManager(context.Background(), db1)
	if err != nil {
		t.Fatal(err)
	}
	m2, c2, err := NewSetupManager(context.Background(), db2)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i, v := range []struct {
		m          *SetupManager
		code, name string
	}{{m1, c1, "first"}, {m2, c2, "second"}} {
		wg.Add(1)
		go func(i int, v struct {
			m          *SetupManager
			code, name string
		}) {
			defer wg.Done()
			<-start
			e := v.m.Claim(context.Background(), v.code, admin.User{Username: v.name, DisplayName: v.name, Role: "admin", SchedulingWeight: 1, MaxQueued: 10, MaxActiveContended: 1}, "long enough password")
			results <- e
		}(i, v)
	}
	close(start)
	wg.Wait()
	close(results)
	success, failed := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else {
			failed++
		}
	}
	var n int
	if err = db1.QueryRow(`SELECT COUNT(*) FROM users WHERE role='admin'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if success != 1 || failed != 1 || n != 1 {
		t.Fatalf("success=%d failed=%d admins=%d", success, failed, n)
	}
}
