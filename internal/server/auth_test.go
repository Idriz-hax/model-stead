package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
