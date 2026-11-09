package server

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/admin"
	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/Idriz-hax/model-stead/internal/database"
	"github.com/Idriz-hax/model-stead/internal/history"
	"github.com/Idriz-hax/model-stead/internal/observability"
	"github.com/Idriz-hax/model-stead/internal/scheduler"
)

const adminOrigin = "https://router.test"

func adminFixture(t *testing.T) (http.Handler, *databaseFixture) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{
		`INSERT INTO users(id,username,display_name,role,status) VALUES('admin','admin','Admin','admin','active')`,
		`INSERT INTO users(id,username,display_name,role,status,must_change_password) VALUES('user','user','User','user','active',0)`,
		`INSERT INTO users(id,username,display_name,role,status,must_change_password) VALUES('forced','forced','Forced','admin','active',1)`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	settings := admin.SchedulerSettings{Policy: "weighted_round_robin", GlobalCapacity: 2, DefaultWeight: 1, MaxQueued: 20, MaxActiveContended: 1, GlobalMaxQueued: 100, MaxWait: time.Minute, WorkConserving: true, CancellationGrace: time.Second}
	if err := (admin.Store{DB: db}).SeedSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(scheduler.Config{Capacity: 2, DefaultWeight: 1, DefaultMaxQueued: 20, DefaultMaxActiveWhenContended: 1, GlobalMaxQueued: 100, MaxWait: time.Minute, WorkConserving: true, CancellationGrace: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return NewWithSchedulerConfig(testFiles, db, GatewayConfig{}, AuthConfig{PublicOrigin: adminOrigin}, SchedulerConfig{Scheduler: sched, History: &history.Store{DB: db}}), &databaseFixture{db: db, sched: sched}
}

// databaseFixture deliberately stays test-local and exposes only setup needed by route tests.
type databaseFixture struct {
	db    *sql.DB
	sched *scheduler.Scheduler
}

func adminCall(t *testing.T, h http.Handler, db *sql.DB, user, method, path, body string, mutate, withOrigin, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		session, err := auth.CreateSession(context.Background(), db, user, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
		if withCSRF {
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
		}
	}
	if withOrigin {
		req.Header.Set("Origin", adminOrigin)
	}
	h.ServeHTTP(w, req)
	return w
}

func adminHandlerWithRegistry(db *sql.DB, sched *scheduler.Scheduler, requests *requestRegistry) http.Handler {
	mux := http.NewServeMux()
	registerAdmin(mux, db, AuthConfig{PublicOrigin: adminOrigin}, sched, requests, observability.New(time.Now(), false))
	return mux
}

func TestAdminAuthorizationAndCSRF(t *testing.T) {
	h, fx := adminFixture(t)
	me := adminCall(t, h, fx.db, "forced", "GET", "/api/v1/auth/me", "", false, false, false)
	if me.Code != 200 || !strings.Contains(me.Body.String(), `"must_change_password":true`) {
		t.Fatalf("forced user /me response: %d %s", me.Code, me.Body.String())
	}
	apiKey, _, err := auth.CreateAPIKey(context.Background(), fx.db, "admin", "dashboard-test")
	if err != nil {
		t.Fatal(err)
	}
	apiRequest := httptest.NewRequest("GET", "/api/v1/admin/overview", nil)
	apiRequest.Header.Set("Authorization", "Bearer "+apiKey)
	apiResponse := httptest.NewRecorder()
	h.ServeHTTP(apiResponse, apiRequest)
	if apiResponse.Code != 401 {
		t.Fatalf("API key must not authorize admin route: %d", apiResponse.Code)
	}
	for _, tc := range []struct {
		user string
		want int
	}{{"", 401}, {"user", 403}, {"forced", 403}, {"admin", 200}} {
		w := adminCall(t, h, fx.db, tc.user, "GET", "/api/v1/admin/overview", "", false, false, false)
		if w.Code != tc.want {
			t.Errorf("user %q got %d want %d: %s", tc.user, w.Code, tc.want, w.Body.String())
		}
	}
	for _, tc := range []struct {
		origin, csrf bool
		want         int
	}{{false, true, 403}, {true, false, 403}, {true, true, 201}} {
		w := adminCall(t, h, fx.db, "admin", "POST", "/api/v1/admin/devices", `{"label":"x","ip_address":"127.0.0.1"}`, true, tc.origin, tc.csrf)
		if w.Code != tc.want {
			t.Errorf("origin=%v csrf=%v got %d want %d", tc.origin, tc.csrf, w.Code, tc.want)
		}
	}
}

func TestRuntimeEndpointUsesAdminSessionGuard(t *testing.T) {
	h, fx := adminFixture(t)
	for _, tc := range []struct {
		user string
		code int
	}{{"", 401}, {"user", 403}, {"forced", 403}, {"admin", 200}} {
		w := adminCall(t, h, fx.db, tc.user, "GET", "/api/v1/admin/runtime", "", false, false, false)
		if w.Code != tc.code {
			t.Errorf("user=%q status=%d want=%d", tc.user, w.Code, tc.code)
		}
		if tc.user == "admin" {
			var snapshot map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"enabled", "status", "healthy", "configured_models", "running_models", "sampled_at"} {
				if _, ok := snapshot[field]; !ok {
					t.Errorf("disabled snapshot missing %s", field)
				}
			}
			if string(snapshot["status"]) != "\"disabled\"" || string(snapshot["enabled"]) != "false" {
				t.Errorf("disabled snapshot=%s", w.Body.String())
			}
		}
	}
}

func TestAdminKeyRevokeAndSchedulerValidation(t *testing.T) {
	h, fx := adminFixture(t)
	key, _, err := (admin.Store{DB: fx.db}).CreateAPIKey(context.Background(), "admin", "user", "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	w := adminCall(t, h, fx.db, "admin", "POST", "/api/v1/admin/api-keys/"+key.ID+"/revoke", "", true, true, true)
	if w.Code != 200 {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	var status string
	if err = fx.db.QueryRow(`SELECT status FROM api_keys WHERE id=?`, key.ID).Scan(&status); err != nil || status != "revoked" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	bad := `{"policy":"weighted_round_robin","global_capacity":2,"default_weight":1,"max_queued":20,"max_active_when_contended":3,"global_max_queued":100,"max_wait":60000000000,"work_conserving":true,"cancellation_grace":1000000000}`
	w = adminCall(t, h, fx.db, "admin", "PUT", "/api/v1/admin/scheduler", bad, true, true, true)
	if w.Code != 400 {
		t.Fatalf("invalid scheduler update status %d", w.Code)
	}
	var capacity int
	if err = fx.db.QueryRow(`SELECT global_capacity FROM scheduler_settings WHERE id=1`).Scan(&capacity); err != nil || capacity != 2 {
		t.Fatalf("persisted capacity=%d err=%v", capacity, err)
	}
}

func TestConcurrentAdminSchedulerUpdatesStayConsistent(t *testing.T) {
	h, fx := adminFixture(t)
	configs := []admin.SchedulerSettings{
		{Policy: "weighted_round_robin", GlobalCapacity: 3, DefaultWeight: 2, MaxQueued: 21, MaxActiveContended: 2, GlobalMaxQueued: 101, MaxWait: 2 * time.Minute, WorkConserving: false, CancellationGrace: 2 * time.Second},
		{Policy: "weighted_round_robin", GlobalCapacity: 4, DefaultWeight: 3, MaxQueued: 22, MaxActiveContended: 3, GlobalMaxQueued: 102, MaxWait: 3 * time.Minute, WorkConserving: false, CancellationGrace: 3 * time.Second},
	}
	sessions := make([]auth.Session, len(configs))
	for i := range sessions {
		var err error
		sessions[i], err = auth.CreateSession(context.Background(), fx.db, "admin", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i, config := range configs {
		config := config
		session := sessions[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			body, _ := json.Marshal(config)
			req := httptest.NewRequest("PUT", "/api/v1/admin/scheduler", bytes.NewReader(body))
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
			req.Header.Set("Origin", adminOrigin)
			req.Header.Set("X-CSRF-Token", session.CSRFToken)
			got := httptest.NewRecorder()
			h.ServeHTTP(got, req)
			if got.Code != 200 {
				t.Errorf("scheduler update status %d: %s", got.Code, got.Body.String())
			}
		}()
	}
	wg.Wait()

	var persisted admin.SchedulerSettings
	persisted, err := (admin.Store{DB: fx.db}).Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := fx.sched.Snapshot()
	if snapshot.Capacity != persisted.GlobalCapacity {
		t.Fatalf("persisted capacity %d differs from scheduler snapshot %d", persisted.GlobalCapacity, snapshot.Capacity)
	}
	match := false
	for _, config := range configs {
		if config.GlobalCapacity == snapshot.Capacity && config == persisted {
			match = true
		}
	}
	if !match {
		t.Fatalf("persisted settings %+v do not match scheduler configuration for capacity %d", persisted, snapshot.Capacity)
	}
}

func TestAdminRequestPaginationAndCancelAuditOrdering(t *testing.T) {
	_, fx := adminFixture(t)
	registry := newRequestRegistry()
	cancelled := make(chan struct{}, 1)
	for _, id := range []string{"r1", "r2"} {
		if !registry.register(liveRequest{ID: id, UserID: "user", Endpoint: "/v1/chat/completions", cancel: func() { cancelled <- struct{}{} }}) {
			t.Fatal("register failed")
		}
	}
	h := adminHandlerWithRegistry(fx.db, fx.sched, registry)
	w := adminCall(t, h, fx.db, "admin", "GET", "/api/v1/admin/requests?limit=1&offset=1", "", false, false, false)
	var page struct {
		Items                []map[string]any `json:"items"`
		Total, Limit, Offset int
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Total != 2 || page.Limit != 1 || page.Offset != 1 {
		t.Fatalf("bad bounded request page: %d %s", w.Code, w.Body.String())
	}
	if got := adminCall(t, h, fx.db, "admin", "GET", "/api/v1/admin/requests?limit=101", "", false, false, false); got.Code != 400 {
		t.Fatalf("limit cap status %d", got.Code)
	}
	if _, err := fx.db.Exec(`CREATE TRIGGER fail_request_audit BEFORE INSERT ON audit_events WHEN NEW.action='request.cancel' BEGIN SELECT RAISE(ABORT,'fail'); END`); err != nil {
		t.Fatal(err)
	}
	got := adminCall(t, h, fx.db, "admin", "POST", "/api/v1/admin/requests/r1/cancel", "", true, true, true)
	if got.Code != 500 {
		t.Fatalf("audit failure status %d", got.Code)
	}
	select {
	case <-cancelled:
		t.Fatal("request canceled despite failed audit")
	default:
	}
	if _, err := fx.db.Exec(`DROP TRIGGER fail_request_audit`); err != nil {
		t.Fatal(err)
	}
	got = adminCall(t, h, fx.db, "admin", "POST", "/api/v1/admin/requests/r1/cancel", "", true, true, true)
	if got.Code != 200 {
		t.Fatalf("cancel status %d: %s", got.Code, got.Body.String())
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("request was not canceled")
	}
}

func TestAdminLiveQueueMetadata(t *testing.T) {
	_, fx := adminFixture(t)
	registry := newRequestRegistry()
	registry.register(liveRequest{ID: "queued", UserID: "user"})
	registry.register(liveRequest{ID: "active", UserID: "user"})
	registry.mu.Lock()
	registry.entries["queued"].EnqueuedAt = time.Now().Add(-5 * time.Second)
	registry.entries["active"].State = "active"
	registry.entries["active"].EnqueuedAt = time.Now().Add(-9 * time.Second)
	registry.entries["active"].StartedAt = time.Now().Add(-4 * time.Second)
	registry.mu.Unlock()
	h := adminHandlerWithRegistry(fx.db, fx.sched, registry)

	users := adminCall(t, h, fx.db, "admin", "GET", "/api/v1/admin/users", "", false, false, false)
	var userResult struct {
		Items []map[string]any `json:"items"`
	}
	if users.Code != 200 || json.Unmarshal(users.Body.Bytes(), &userResult) != nil {
		t.Fatalf("users response %d: %s", users.Code, users.Body.String())
	}
	var user map[string]any
	for _, item := range userResult.Items {
		if item["id"] == "user" {
			user = item
			break
		}
	}
	if user == nil || user["queued_requests"] != float64(1) || user["active_requests"] != float64(1) {
		t.Fatalf("unexpected user request counts: %#v", user)
	}

	requests := adminCall(t, h, fx.db, "admin", "GET", "/api/v1/admin/requests", "", false, false, false)
	var requestResult struct {
		Items []map[string]any `json:"items"`
	}
	if requests.Code != 200 || json.Unmarshal(requests.Body.Bytes(), &requestResult) != nil {
		t.Fatalf("requests response %d: %s", requests.Code, requests.Body.String())
	}
	waits := make(map[string]float64)
	for _, item := range requestResult.Items {
		waits[item["id"].(string)] = item["queue_wait_seconds"].(float64)
	}
	if waits["queued"] < 5 || waits["active"] < 4 || waits["active"] > 5.1 {
		t.Fatalf("unexpected queue waits in seconds: %#v", waits)
	}

	overview := adminCall(t, h, fx.db, "admin", "GET", "/api/v1/admin/overview", "", false, false, false)
	var overviewResult struct {
		Scheduler map[string]any `json:"scheduler"`
	}
	if overview.Code != 200 || json.Unmarshal(overview.Body.Bytes(), &overviewResult) != nil {
		t.Fatalf("overview response %d: %s", overview.Code, overview.Body.String())
	}
	if oldest := overviewResult.Scheduler["oldest_queued_wait_seconds"].(float64); oldest < 5 {
		t.Fatalf("oldest queued wait was %.2f seconds", oldest)
	}
}

func TestAdminDisableCancelsRequestsAndFilteredAudit(t *testing.T) {
	_, fx := adminFixture(t)
	registry := newRequestRegistry()
	cancelled := make(chan struct{}, 1)
	if !registry.register(liveRequest{ID: "live", UserID: "user", cancel: func() { cancelled <- struct{}{} }}) {
		t.Fatal("register failed")
	}
	h := adminHandlerWithRegistry(fx.db, fx.sched, registry)
	w := adminCall(t, h, fx.db, "admin", "POST", "/api/v1/admin/users/user/disable", "", true, true, true)
	if w.Code != 200 {
		t.Fatalf("disable: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("disable did not cancel work")
	}
	w = adminCall(t, h, fx.db, "admin", "GET", "/api/v1/admin/audit?actor=admin&action=user.disable&target=user&limit=10", "", false, false, false)
	var result struct {
		Items []admin.AuditEvent `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Items) != 1 {
		t.Fatalf("audit filter response %d: %s", w.Code, w.Body.String())
	}
	if result.Items[0].ActorID != "admin" || result.Items[0].Action != "user.disable" || result.Items[0].TargetID != "user" {
		t.Fatalf("wrong filtered event: %+v", result.Items[0])
	}
	w = adminCall(t, h, fx.db, "admin", "POST", "/api/v1/admin/users/user/enable", "", true, true, true)
	if w.Code != 200 {
		t.Fatalf("enable: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminSSEInitialSnapshotAndRequestChanges(t *testing.T) {
	h, fx := adminFixture(t)
	server := httptest.NewServer(h)
	defer server.Close()
	session, err := auth.CreateSession(context.Background(), fx.db, "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/admin/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session.Token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var frame strings.Builder
	for {
		line, e := reader.ReadString('\n')
		if e != nil {
			t.Fatal(e)
		}
		frame.WriteString(line)
		if line == "\n" {
			break
		}
	}
	if !strings.Contains(frame.String(), `"capacity":2`) {
		t.Fatalf("unexpected SSE snapshot: %s", frame.String())
	}
	registry := newRequestRegistry()
	changes, stop := registry.WatchChanges()
	defer stop()
	registry.register(liveRequest{ID: "r", cancel: func() {}})
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("registry change not broadcast")
	}
	registry.finish("r")
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("request finish not broadcast")
	}
}

func TestAdminSnapshotUsesSnakeCase(t *testing.T) {
	s := scheduler.Snapshot{Capacity: 3, Active: 1, Reservations: 2, Queued: 4, Available: 0, DispatchTotal: 9, Identities: map[string]scheduler.IdentitySnapshot{"x": {Queued: 2, Active: 1, Weight: 1}}}
	b, err := json.Marshal(schedulerSnapshot(s))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"capacity"`, `"dispatch_total"`, `"identities"`} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("missing %s in %s", key, b)
		}
	}
	if strings.Contains(string(b), `"Capacity"`) {
		t.Fatalf("uppercase scheduler fields leaked: %s", b)
	}
}
