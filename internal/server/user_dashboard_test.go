package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/Idriz-hax/model-stead/internal/history"
)

func TestUserDashboardAnonymous(t *testing.T) {
	h, _ := adminFixture(t)
	for _, path := range []string{"/api/v1/me/usage", "/api/v1/me/requests"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v", path, w.Code, w.Header())
		}
	}
}

func TestUserDashboardSessionAndOwnerScope(t *testing.T) {
	h, fx := adminFixture(t)
	store := &history.Store{DB: fx.db}
	for _, row := range []history.Record{
		{RequestID: "user-owned", UserID: "user", Endpoint: "/v1/chat/completions", EnqueuedAt: time.Now().UTC().Add(-time.Second), Outcome: "completed", UsageStatus: "unavailable"},
		{RequestID: "admin-owned", UserID: "admin", Endpoint: "/v1/chat/completions", EnqueuedAt: time.Now().UTC(), Outcome: "completed", UsageStatus: "unavailable"},
	} {
		if err := store.Create(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	session, err := auth.CreateSession(context.Background(), fx.db, "user", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/me/requests?user_id=admin", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var page struct {
		Items []struct {
			RequestID string `json:"request_id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].RequestID != "user-owned" {
		t.Fatalf("cross-owner list returned %+v", page.Items)
	}
	usage := httptest.NewRequest("GET", "/api/v1/me/usage", nil)
	usage.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
	usageResponse := httptest.NewRecorder()
	h.ServeHTTP(usageResponse, usage)
	if usageResponse.Code != http.StatusOK {
		t.Fatalf("default usage range status %d: %s", usageResponse.Code, usageResponse.Body.String())
	}
	var summary struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
		Requests     int64  `json:"requests"`
	}
	if err := json.Unmarshal(usageResponse.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.InputTokens != nil || summary.OutputTokens != nil || summary.Requests != 1 {
		t.Fatalf("unknown token counts must remain unavailable: %+v", summary)
	}
}

func TestUserDashboardSessionAuthorizationGates(t *testing.T) {
	h, fx := adminFixture(t)
	forced := adminCall(t, h, fx.db, "forced", http.MethodGet, "/api/v1/me/usage", "", false, false, false)
	if forced.Code != http.StatusForbidden {
		t.Fatalf("forced session status=%d", forced.Code)
	}
	admin := adminCall(t, h, fx.db, "admin", http.MethodGet, "/api/v1/me/usage", "", false, false, false)
	if admin.Code != http.StatusOK || admin.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("admin personal usage status=%d cache=%q body=%s", admin.Code, admin.Header().Get("Cache-Control"), admin.Body.String())
	}
	key, _, err := auth.CreateAPIKey(context.Background(), fx.db, "admin", "personal-test")
	if err != nil {
		t.Fatal(err)
	}
	apiOnly := httptest.NewRequest(http.MethodGet, "/api/v1/me/usage", nil)
	apiOnly.Header.Set("Authorization", "Bearer "+key)
	apiResponse := httptest.NewRecorder()
	h.ServeHTTP(apiResponse, apiOnly)
	if apiResponse.Code != http.StatusUnauthorized {
		t.Fatalf("API-key-only status=%d", apiResponse.Code)
	}
	session, err := auth.CreateSession(context.Background(), fx.db, "user", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.db.Exec(`UPDATE users SET status='disabled' WHERE id='user'`); err != nil {
		t.Fatal(err)
	}
	disabled := httptest.NewRequest(http.MethodGet, "/api/v1/me/usage", nil)
	disabled.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
	disabledResponse := httptest.NewRecorder()
	h.ServeHTTP(disabledResponse, disabled)
	if disabledResponse.Code != http.StatusUnauthorized {
		t.Fatalf("disabled session status=%d", disabledResponse.Code)
	}
}

func TestUserDashboardCancelCSRFAndOwnerDisclosure(t *testing.T) {
	_, fx := adminFixture(t)
	registry := newRequestRegistry()
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	defer ownerCancel()
	if !registry.register(liveRequest{ID: "other-owner", UserID: "admin", cancel: ownerCancel}) {
		t.Fatal("register failed")
	}
	userCtx, userCancel := context.WithCancel(context.Background())
	defer userCancel()
	if !registry.register(liveRequest{ID: "own", UserID: "user", cancel: userCancel}) {
		t.Fatal("register failed")
	}
	mux := http.NewServeMux()
	registerUserDashboard(mux, fx.db, AuthConfig{PublicOrigin: adminOrigin}, &history.Store{DB: fx.db}, registry)
	session, err := auth.CreateSession(context.Background(), fx.db, "user", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	call := func(id, origin, csrf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/me/requests/"+id+"/cancel", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		return w
	}
	for _, tc := range []struct{ origin, csrf string }{{"", session.CSRFToken}, {adminOrigin, ""}, {"https://other.test", session.CSRFToken}} {
		if got := call("own", tc.origin, tc.csrf); got.Code != http.StatusForbidden {
			t.Fatalf("CSRF/origin status=%d", got.Code)
		}
	}
	if userCtx.Err() != nil {
		t.Fatal("invalid CSRF canceled request")
	}
	unknown := call("missing", adminOrigin, session.CSRFToken)
	crossOwner := call("other-owner", adminOrigin, session.CSRFToken)
	if unknown.Code != http.StatusNotFound || crossOwner.Code != http.StatusNotFound || unknown.Body.String() != crossOwner.Body.String() {
		t.Fatalf("cancel responses unknown=%d %q cross-owner=%d %q", unknown.Code, unknown.Body.String(), crossOwner.Code, crossOwner.Body.String())
	}
	if ownerCtx.Err() != nil {
		t.Fatal("cross-owner cancellation affected request")
	}
	if got := call("own", adminOrigin, session.CSRFToken); got.Code != http.StatusOK || userCtx.Err() != context.Canceled {
		t.Fatalf("owner cancellation status=%d err=%v", got.Code, userCtx.Err())
	}
}

func TestUserDashboardQueryValidationAndStoreErrors(t *testing.T) {
	h, fx := adminFixture(t)
	session, err := auth.CreateSession(context.Background(), fx.db, "user", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{
		"/api/v1/me/requests?limit=0",
		"/api/v1/me/requests?offset=-1",
		"/api/v1/me/requests?endpoint=/v1/messages/count_tokens",
		"/api/v1/me/requests?outcome=unknown",
		"/api/v1/me/requests?search=" + strings.Repeat("x", 129),
	} {
		if got := get(path); got.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s", path, got.Code, got.Body.String())
		}
	}
	if _, err := fx.db.Exec(`DROP TABLE request_history`); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/me/requests", "/api/v1/me/usage"} {
		if got := get(path); got.Code != http.StatusServiceUnavailable {
			t.Errorf("store failure %s: status=%d body=%s", path, got.Code, got.Body.String())
		}
	}
}
