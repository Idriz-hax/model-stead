package server

import (
	"bufio"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/Idriz-hax/model-stead/internal/database"
	"github.com/Idriz-hax/model-stead/internal/history"
	"github.com/Idriz-hax/model-stead/internal/scheduler"
)

//go:embed dist/*
var testFiles embed.FS

func gatewayAuth(t *testing.T, ips ...string) (*sql.DB, *auth.Resolver, string) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO users(id,username,display_name,role,status) VALUES('u','user','User','user','active')`); err != nil {
		t.Fatal(err)
	}
	for i, ip := range ips {
		if _, err := db.Exec(`INSERT INTO devices(id,user_id,label,ip_address,trusted) VALUES(?,?,?, ?,1)`, fmt.Sprintf("d%d", i), "u", "test", ip); err != nil {
			t.Fatal(err)
		}
	}
	key, _, err := auth.CreateAPIKey(context.Background(), db, "u", "test")
	if err != nil {
		t.Fatal(err)
	}
	return db, auth.NewResolver(db, auth.ResolverConfig{IPFallbackEnabled: true}), key
}

func authenticatedGateway(t *testing.T, cfg GatewayConfig, ips ...string) http.Handler {
	db, resolver, _ := gatewayAuth(t, ips...)
	return NewWithConfig(testFiles, db, cfg, AuthConfig{Resolver: resolver})
}

func addGatewayKey(req *http.Request, key string) {
	req.Header.Set("Authorization", "Bearer "+key)
}

func TestLifecycleRecorderPersistsCompletedUsage(t *testing.T) {
	db, _, _ := gatewayAuth(t)
	store := &history.Store{DB: db}
	now := time.Now().UTC()
	if err := store.Create(context.Background(), history.Record{RequestID: "lifecycle", UserID: "u", Endpoint: "/v1/chat/completions", EnqueuedAt: now, Outcome: "queued", UsageStatus: "unavailable"}); err != nil {
		t.Fatal(err)
	}
	in, out, total, status := int64(12), int64(4), int64(16), http.StatusOK
	recorder := &lifecycleRecorder{store: store, userID: "u", requestID: "lifecycle", status: &status, usage: TokenUsage{Input: &in, Output: &out, Total: &total, Status: "reported"}, readDone: true, proxyReturned: true}
	lifecycle := recorder.snapshot()
	recorder.update(lifecycle)
	rows, count, err := store.ListPage(context.Background(), "u", history.Query{})
	if err != nil || count != 1 || rows[0].Outcome != "completed" || rows[0].ResponseStatus == nil || *rows[0].ResponseStatus != status || rows[0].InputTokens == nil || *rows[0].InputTokens != in || rows[0].UsageStatus != "reported" {
		t.Fatalf("rows=%+v count=%d err=%v", rows, count, err)
	}
}

func TestLifecycleRecorderDoesNotInferSuccessFromEOFAlone(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		status                                   *int
		readDone, proxyReturned, requestCanceled bool
		readError                                bool
		want                                     string
	}{
		{name: "upstream http error", status: intPtrForTest(http.StatusBadGateway), readDone: true, proxyReturned: true, want: "upstream_error"},
		{name: "upstream body read error", status: intPtrForTest(http.StatusOK), readError: true, proxyReturned: true, want: "upstream_error"},
		{name: "incomplete response", status: intPtrForTest(http.StatusOK), proxyReturned: true, want: "interrupted"},
		{name: "canceled request", requestCanceled: true, want: "cancelled"},
		{name: "completed response", status: intPtrForTest(http.StatusOK), readDone: true, proxyReturned: true, want: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &lifecycleRecorder{status: tc.status, readDone: tc.readDone, readError: tc.readError, proxyReturned: tc.proxyReturned, requestCanceled: tc.requestCanceled}
			got := recorder.snapshot()
			if got.Outcome != tc.want {
				t.Fatalf("outcome=%q, want %q", got.Outcome, tc.want)
			}
		})
	}
}

func intPtrForTest(n int) *int { return &n }

func TestObservedBodyRecordsUpstreamReadError(t *testing.T) {
	readErr := errors.New("upstream body read failed")
	recorder := &lifecycleRecorder{status: intPtrForTest(http.StatusOK)}
	jsonObserver := newJSONUsageObserver("/v1/chat/completions")
	jsonObserver.Feed([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":3}}`))
	body := &observedBody{ReadCloser: io.NopCloser(errorReader{err: readErr}), recorder: recorder, ctx: context.Background(), json: jsonObserver}
	if n, err := body.Read(make([]byte, 16)); n != 0 || !errors.Is(err, readErr) {
		t.Fatalf("read=(%d,%v), want upstream error", n, err)
	}
	recorder.mu.Lock()
	recorder.proxyReturned = true
	recorder.mu.Unlock()
	got := recorder.snapshot()
	if got.Outcome != "upstream_error" || got.UsageStatus != "unavailable" {
		t.Fatalf("lifecycle=%+v, want upstream_error with unavailable JSON usage", got)
	}
}

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestGatewayPersistsOwnerScopedProviderUsage(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`)
	}))
	defer up.Close()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	store := &history.Store{DB: db}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 4, GlobalMaxQueued: 4, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 4096, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched, History: store})
	front := httptest.NewServer(h)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"  model-x  ","messages":[{"content":"private prompt"}]}`))
	addGatewayKey(req, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || string(responseBody) != `{"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}` {
		t.Fatalf("response status=%d body=%q err=%v", resp.StatusCode, responseBody, err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		rows, count, err := store.ListPage(context.Background(), "u", history.Query{})
		if err != nil || count != 1 || len(rows) != 1 {
			t.Fatalf("history count=%d rows=%+v err=%v", count, rows, err)
		}
		row := rows[0]
		if row.Outcome == "completed" {
			if row.RequestedModel == nil || *row.RequestedModel != "model-x" || row.ResponseStatus == nil || *row.ResponseStatus != http.StatusOK || row.InputTokens == nil || *row.InputTokens != 8 || row.OutputTokens == nil || *row.OutputTokens != 3 || row.TotalTokens == nil || *row.TotalTokens != 11 || row.UsageStatus != "reported" {
				t.Fatalf("history row: %+v", row)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history did not finalize: %+v", row)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGatewayPersistsTransportFailureWithoutFailingRequest(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := closed.URL
	closed.Close()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	store := &history.Store{DB: db}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 4, GlobalMaxQueued: 4, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: baseURL, MaxBodyBytes: 1024, SpoolBudgetBytes: 4096, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched, History: store})
	front := httptest.NewServer(h)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	addGatewayKey(req, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	deadline := time.Now().Add(time.Second)
	for {
		rows, _, err := store.ListPage(context.Background(), "u", history.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Outcome == "upstream_error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history did not record transport failure: %+v", rows)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHistoryWriteFailureDoesNotFailInference(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer up.Close()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	if _, err := db.Exec(`DROP TABLE request_history`); err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 4, GlobalMaxQueued: 4, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 4096, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched, History: &history.Store{DB: db}})
	front := httptest.NewServer(h)
	defer front.Close()
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	addGatewayKey(req, key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("inference status=%d body=%q error=%v", resp.StatusCode, body, readErr)
	}
}

func TestHealthAndStatic(t *testing.T) {
	h := New(testFiles)
	for _, path := range []string{"/health", "/ready", "/"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Errorf("%s: got %d", path, w.Code)
		}
		if path != "/" && w.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: content type = %q, want application/json", path, w.Header().Get("Content-Type"))
		}
	}
}

func TestSSEStreamsBeforeUpstreamCompletes(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	upstreamDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = fmt.Fprint(w, "data: done\n\n")
		close(upstreamDone)
	}))
	defer up.Close()
	front := httptest.NewServer(authenticatedGateway(t, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, "127.0.0.1"))
	defer front.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: first\n" {
		t.Fatalf("first stream bytes %q, %v", line, err)
	}
	select {
	case <-upstreamDone:
		t.Fatal("upstream completed before the test released it")
	default:
	}
	unblock()
	_, _ = io.ReadAll(resp.Body)
}

func TestClientCancellationCancelsUpstream(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	store := &history.Store{DB: db}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 4, GlobalMaxQueued: 4, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched, History: store}))
	defer func() { releaseOnce.Do(func() { close(release) }); front.Close(); up.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	addGatewayKey(req, key)
	done := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not drain the request body")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream context was not canceled")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("client request did not complete after cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for {
		rows, _, err := store.ListPage(context.Background(), "u", history.Query{})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && rows[0].Outcome == "cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("history did not record client cancellation: %+v", rows)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestQueuedRequestTimeoutIsPersisted(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer func() { once.Do(func() { close(release) }); up.Close() }()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	store := &history.Store{DB: db}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 1, GlobalMaxQueued: 1, MaxWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 4096, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched, History: store}))
	defer front.Close()
	request := func() *http.Request {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		addGatewayKey(req, key)
		return req
	}
	first := make(chan struct{})
	go func() {
		defer close(first)
		resp, e := http.DefaultClient.Do(request())
		if e == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not reach upstream")
	}
	resp, err := http.DefaultClient.Do(request())
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("queued timeout status=%d", resp.StatusCode)
	}
	rows, _, err := store.ListPage(context.Background(), "u", history.Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Outcome == "timed_out" {
			found = true
		}
	}
	if !found {
		t.Fatalf("timed_out history row missing: %+v", rows)
	}
	once.Do(func() { close(release) })
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("first request failed to finish")
	}
}

func TestUpstreamErrorStatusAndBodyPassThrough(t *testing.T) {
	for _, code := range []int{429, 502} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "upstream failure\n")
			}))
			defer up.Close()
			front := httptest.NewServer(authenticatedGateway(t, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, "127.0.0.1"))
			defer front.Close()
			resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != code || string(body) != "upstream failure\n" {
				t.Fatalf("got %d %q", resp.StatusCode, body)
			}
		})
	}
}

func TestOversizedBodyRejectedBeforeUpstream(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	front := httptest.NewServer(authenticatedGateway(t, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 16, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, "127.0.0.1"))
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m","too_long":"0123456789"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 || hits.Load() != 0 {
		t.Fatalf("status %d upstream hits %d", resp.StatusCode, hits.Load())
	}
}

func TestModelsRejectsRequestBody(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	front := httptest.NewServer(authenticatedGateway(t, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, "127.0.0.1"))
	defer front.Close()
	req, err := http.NewRequest(http.MethodGet, front.URL+"/v1/models", strings.NewReader("unexpected"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || hits.Load() != 0 {
		t.Fatalf("status %d, upstream hits %d", resp.StatusCode, hits.Load())
	}
}

func TestHTTPServerBoundsRequestAndIdleConnectionsWithoutClippingStreams(t *testing.T) {
	s := NewHTTPServer(":8080", New(testFiles))
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.IdleTimeout <= 0 {
		t.Fatalf("missing HTTP read/idle timeouts: %+v", s)
	}
	if s.WriteTimeout != 0 {
		t.Fatalf("write timeout would clip long-lived inference streams: %s", s.WriteTimeout)
	}
}

func TestSpoolMalformedDoesNotConsumeBudgetAndCloseIsIdempotent(t *testing.T) {
	cfg := GatewayConfig{MaxBodyBytes: 1024, SpoolBudgetBytes: 64, SpoolDir: t.TempDir()}
	for _, body := range []string{`{`, `{ "model":"m" } {}`, `{ "model":"" }`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if _, err := spoolRequest(httptest.NewRecorder(), r, cfg); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"  m  "}`))
	model, err := spoolRequest(httptest.NewRecorder(), r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if model != "m" {
		t.Fatalf("model = %q, want m", model)
	}
	b := r.Body.(*spoolBody)
	info, err := os.Stat(b.name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("spool mode: %o", info.Mode().Perm())
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if got := spoolUsed.Load(); got != 0 {
		t.Fatalf("reservation leaked: %d", got)
	}
	if _, err := os.Stat(b.name); !os.IsNotExist(err) {
		t.Fatalf("spool remains after close: %v", err)
	}
	if entries, err := filepath.Glob(filepath.Join(cfg.SpoolDir, "router-spool-*")); err != nil || len(entries) != 0 {
		t.Fatalf("spool files: %v, %v", entries, err)
	}
}

func TestDefaultSpoolUsesPrivateChildWithoutChangingTempRoot(t *testing.T) {
	tempRoot := filepath.Join(t.TempDir(), "shared-temp")
	if err := os.Mkdir(tempRoot, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tempRoot)
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"m"}`))
	if _, err := spoolRequest(httptest.NewRecorder(), r, GatewayConfig{MaxBodyBytes: 1024, SpoolBudgetBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("shared temp root permissions changed to %o", info.Mode().Perm())
	}
	if err := r.Body.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("default spool directory remains: %v", entries)
	}
}

type gatedReader struct {
	once    sync.Once
	gate    <-chan struct{}
	started chan<- struct{}
	data    []byte
	calls   int
}

func (r *gatedReader) Read(p []byte) (int, error) {
	if r.calls > 0 {
		r.once.Do(func() { close(r.started); <-r.gate })
	}
	r.calls++
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(p) > 32 {
		p = p[:32]
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestSpoolBudgetReservedDuringConcurrentWrites(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	body := `{"model":"m","x":"` + strings.Repeat("x", 64) + `"}`
	cfg := GatewayConfig{MaxBodyBytes: 1024, SpoolBudgetBytes: 40, SpoolDir: t.TempDir()}
	r := httptest.NewRequest("POST", "/", &gatedReader{gate: gate, started: started, data: []byte(body)})
	done := make(chan error, 1)
	go func() { _, err := spoolRequest(httptest.NewRecorder(), r, cfg); done <- err }()
	<-started
	second := httptest.NewRequest("POST", "/", strings.NewReader(`{"model":"m"}`))
	_, err := spoolRequest(httptest.NewRecorder(), second, cfg)
	if !errors.Is(err, errBudget) {
		t.Fatalf("second request: %v", err)
	}
	close(gate)
	if err := <-done; !errors.Is(err, errBudget) {
		t.Fatalf("first request: %v", err)
	}
	if got := spoolUsed.Load(); got != 0 {
		t.Fatalf("reservation leaked: %d", got)
	}
}

func TestGatewayOptInAndRouteIsolation(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer up.Close()
	for _, tc := range []struct {
		enabled bool
		path    string
		method  string
		want    int
	}{
		{false, "/v1/models", "GET", 404}, {true, "/v1/models", "GET", 204}, {true, "/v1/unknown", "GET", 404}, {true, "/v1/models", "POST", 405},
	} {
		var h http.Handler
		if tc.enabled {
			h = authenticatedGateway(t, GatewayConfig{Enabled: true, BaseURL: up.URL, APIKey: "secret"}, "192.0.2.1")
		} else {
			h = NewWithGateway(testFiles, GatewayConfig{})
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Errorf("%v %s: got %d want %d", tc.enabled, tc.path, w.Code, tc.want)
		}
	}
}

func TestEnabledGatewayRequiresResolver(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected gateway without resolver to panic")
		}
	}()
	NewWithConfig(testFiles, nil, GatewayConfig{Enabled: true}, AuthConfig{})
}

func TestMetricsAvailableWithoutGateway(t *testing.T) {
	db, resolver, key := gatewayAuth(t)
	if _, err := db.Exec(`UPDATE users SET role='admin' WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	h := NewWithConfig(testFiles, db, GatewayConfig{}, AuthConfig{Resolver: resolver})
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated metrics status=%d", unauthorized.Code)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	addGatewayKey(req, key)
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "scheduler_capacity") {
		t.Fatalf("status=%d metrics=%q", w.Code, w.Body.String())
	}
}

func TestInferenceMetricsExcludeDiscoveryAndCountTokensWithoutHistory(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer up.Close()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	if _, err := db.Exec(`UPDATE users SET role='admin' WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 4, GlobalMaxQueued: 4, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched})
	for _, tc := range []struct{ method, path string }{{http.MethodPost, "/v1/chat/completions"}, {http.MethodGet, "/v1/models"}, {http.MethodPost, "/v1/messages/count_tokens"}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"m"}`))
		addGatewayKey(req, key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	addGatewayKey(req, key)
	h.ServeHTTP(w, req)
	text := w.Body.String()
	if !strings.Contains(text, `endpoint="/v1/chat/completions",outcome="upstream_error"} 1`) {
		t.Fatalf("missing upstream failure without history: %s", text)
	}
	if strings.Contains(text, "/v1/models") || strings.Contains(text, "/v1/messages/count_tokens") {
		t.Fatalf("excluded route in metrics: %s", text)
	}
}

func scrapeInferenceMetrics(t *testing.T, h http.Handler, key string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	addGatewayKey(req, key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("metrics status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func metricCount(text, endpoint, outcome string) int {
	line := fmt.Sprintf(`router_inference_requests_total{endpoint=%q,outcome=%q} `, endpoint, outcome)
	for _, row := range strings.Split(text, "\n") {
		if strings.HasPrefix(row, line) {
			var count int
			_, _ = fmt.Sscanf(strings.TrimPrefix(row, line), "%d", &count)
			return count
		}
	}
	return 0
}

func TestInferenceCancellationMetricRecordedOnce(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Errorf("read upstream request body: %v", err)
			return
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer up.Close()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	if _, err := db.Exec(`UPDATE users SET role='admin' WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 2, GlobalMaxQueued: 2, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)).WithContext(ctx)
		req.RemoteAddr = "127.0.0.1:1234"
		addGatewayKey(req, key)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("upstream was not reached")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler did not return after cancellation")
	}
	text := scrapeInferenceMetrics(t, h, key)
	if got := metricCount(text, "/v1/chat/completions", "cancelled"); got != 1 {
		t.Fatalf("cancelled count=%d metrics=%s", got, text)
	}
	if got := metricCount(text, "/v1/chat/completions", "completed"); got != 0 {
		t.Fatalf("completed count=%d metrics=%s", got, text)
	}
}

type failingResponseWriter struct{ header http.Header }

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*failingResponseWriter) WriteHeader(int) {}
func (*failingResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("downstream write failed")
}

func TestInferenceInterruptedMetricOnDownstreamWriteFailure(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "response body") }))
	defer up.Close()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	if _, err := db.Exec(`UPDATE users SET role='admin' WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 2, GlobalMaxQueued: 2, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	addGatewayKey(req, key)
	req = req.WithContext(context.WithValue(req.Context(), http.ServerContextKey, &http.Server{}))
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Fatalf("panic=%v, want http.ErrAbortHandler", got)
			}
		}()
		h.ServeHTTP(&failingResponseWriter{}, req)
	}()
	text := scrapeInferenceMetrics(t, h, key)
	if got := metricCount(text, "/v1/chat/completions", "interrupted"); got != 1 {
		t.Fatalf("interrupted count=%d metrics=%s", got, text)
	}
	if got := metricCount(text, "/v1/chat/completions", "completed"); got != 0 {
		t.Fatalf("completed count=%d metrics=%s", got, text)
	}
}

func TestInferenceMetricsCountSuccessRejectionAndQueueTimeout(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer up.Close()
	db, resolver, key := gatewayAuth(t)
	if _, err := db.Exec(`UPDATE users SET role='admin' WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 4, GlobalMaxQueued: 4, MaxWait: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched})

	call := func(apiKey string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		if apiKey != "" {
			addGatewayKey(req, apiKey)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if got := call(key); got.Code != http.StatusNoContent {
		t.Fatalf("success status=%d body=%q", got.Code, got.Body.String())
	}
	if got := call("invalid-key"); got.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key status=%d", got.Code)
	}
	lease, err := sched.Submit(context.Background(), scheduler.Request{ID: "occupy", IdentityID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	lease.MarkDispatched()
	if got := call(key); got.Code != http.StatusTooManyRequests {
		t.Fatalf("queue timeout status=%d", got.Code)
	}
	lease.Release()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	addGatewayKey(req, key)
	h.ServeHTTP(w, req)
	text := w.Body.String()
	for _, want := range []string{
		`endpoint="/v1/chat/completions",outcome="completed"} 1`,
		`endpoint="/v1/chat/completions",outcome="rejected"} 1`,
		`endpoint="/v1/chat/completions",outcome="timed_out"} 1`,
		`router_inference_queue_wait_seconds_count 2`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
}

func TestGatewayRejectsInvalidKeyBeforeSpoolingOrUpstream(t *testing.T) {
	var upstreamHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	db, resolver, _ := gatewayAuth(t)
	h := NewWithConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("x", 128)))
	w := httptest.NewRecorder()
	usedBefore := spoolUsed.Load()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || upstreamHits.Load() != 0 || spoolUsed.Load() != usedBefore {
		t.Fatalf("status=%d upstream=%d spool=%d->%d", w.Code, upstreamHits.Load(), usedBefore, spoolUsed.Load())
	}
}

func TestUnknownAuthAPIPathDoesNotFallThroughToSPA(t *testing.T) {
	db, _, _ := gatewayAuth(t)
	h := NewWithConfig(testFiles, db, GatewayConfig{}, AuthConfig{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/auth/not-a-route", nil))
	if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "<html") {
		t.Fatalf("unexpected API fallback: status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestProxyPreservesPathQueryAndSanitizesCredentials(t *testing.T) {
	got := make(chan *http.Request, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Clone(r.Context()); w.WriteHeader(204) }))
	defer up.Close()
	db, resolver, callerKey := gatewayAuth(t, "192.0.2.1")
	h := NewWithConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL + "/api", APIKey: "router-key"}, AuthConfig{Resolver: resolver})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions?x=a%2Fb", strings.NewReader(`{"model":"m"}`))
	addGatewayKey(req, callerKey)
	req.Header.Set("x-api-key", callerKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	h.ServeHTTP(w, req)
	r := <-got
	if r.URL.String() != "/api/v1/chat/completions?x=a%2Fb" {
		t.Errorf("URL %q", r.URL.String())
	}
	if r.Header.Get("Authorization") != "Bearer router-key" || r.Header.Get("x-api-key") != "" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("headers: %v", r.Header)
	}
}

func TestRemainingPostEndpointsPreservePath(t *testing.T) {
	paths := []string{"/v1/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens", "/v1/embeddings"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, path)
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer up.Close()
			h := authenticatedGateway(t, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, "192.0.2.1")
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"m"}`))
			h.ServeHTTP(w, req)
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
			}
		})
	}
}

func TestProviderToolCallPayloadsPassThroughUnchanged(t *testing.T) {
	tests := []struct {
		path     string
		request  string
		response string
	}{
		{
			path:     "/v1/chat/completions",
			request:  `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":1}"}}]}]}`,
			response: `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function"}]}}]}`,
		},
		{
			path:     "/v1/messages",
			request:  `{"model":"claude","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"id":1}}]}]}`,
			response: `{"content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"id":1}}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream body: %v", err)
				}
				if string(body) != tt.request {
					t.Errorf("request body changed: %s", body)
				}
				_, _ = io.WriteString(w, tt.response)
			}))
			defer up.Close()
			front := httptest.NewServer(authenticatedGateway(t, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 2048, SpoolBudgetBytes: 2048, SpoolDir: t.TempDir()}, "127.0.0.1"))
			defer front.Close()
			resp, err := http.Post(front.URL+tt.path, "application/json", strings.NewReader(tt.request))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.response {
				t.Errorf("response body changed: %s", got)
			}
		})
	}
}

func TestSchedulerQueuesInferenceRoutesAndModelsBypass(t *testing.T) {
	started := make(chan string, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- r.URL.Path
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	sched, err := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 1, GlobalMaxQueued: 1, MaxWait: 4 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	store := &history.Store{DB: db}
	if _, err := db.Exec(`UPDATE users SET role='admin' WHERE id='u'`); err != nil {
		t.Fatal(err)
	}
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 4096, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched, History: store})
	front := httptest.NewServer(h)
	defer front.Close()
	defer releaseOnce.Do(func() { close(release) })
	request := func(path string) <-chan int {
		result := make(chan int, 1)
		go func() {
			req, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{"model":"m"}`))
			addGatewayKey(req, key)
			resp, e := http.DefaultClient.Do(req)
			if e != nil {
				result <- 0
				return
			}
			defer resp.Body.Close()
			result <- resp.StatusCode
		}()
		return result
	}
	first := request("/v1/chat/completions")
	select {
	case p := <-started:
		if p != "/v1/chat/completions" {
			t.Fatalf("first upstream path %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first request did not reach upstream")
	}
	second := request("/v1/embeddings")
	waitForScheduler(t, func() bool { return sched.Snapshot().Queued == 1 })
	select {
	case p := <-started:
		t.Fatalf("queued request reached upstream: %s", p)
	default:
	}
	third := request("/v1/messages")
	select {
	case code := <-third:
		if code != http.StatusTooManyRequests {
			t.Fatalf("queue-full status %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queue-full request did not finish")
	}
	// Models proxy without acquiring scheduler capacity.
	models := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/models", nil)
		addGatewayKey(req, key)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			models <- 0
			return
		}
		defer resp.Body.Close()
		models <- resp.StatusCode
	}()
	select {
	case p := <-started:
		if p != "/v1/models" {
			t.Fatalf("models upstream path %s", p)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("models request blocked on scheduler")
	}
	metrics := httptest.NewRecorder()
	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	addGatewayKey(metricsRequest, key)
	h.ServeHTTP(metrics, metricsRequest)
	for _, line := range []string{"scheduler_capacity 1", "scheduler_active 1", "scheduler_queued 1"} {
		if !strings.Contains(metrics.Body.String(), line) {
			t.Fatalf("missing %q in metrics: %s", line, metrics.Body.String())
		}
	}
	releaseOnce.Do(func() { close(release) })
	if code := <-first; code != http.StatusNoContent {
		t.Fatalf("first status %d", code)
	}
	if code := <-second; code != http.StatusNoContent {
		t.Fatalf("second status %d", code)
	}
	if code := <-models; code != http.StatusNoContent {
		t.Fatalf("models status %d", code)
	}
	rows, count, err := store.ListPage(context.Background(), "u", history.Query{Limit: 10})
	if err != nil || count != 3 {
		t.Fatalf("history count=%d rows=%+v err=%v", count, rows, err)
	}
	rejected := 0
	for _, row := range rows {
		if row.Endpoint == "/v1/messages" && row.Outcome == "rejected" {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("queue rejection outcome missing: %+v", rows)
	}
}

func waitForScheduler(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scheduler state did not settle")
}

func TestSchedulerQueuedCancellationRemovesWork(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	defer releaseOnce.Do(func() { close(release) })
	sched, _ := scheduler.New(scheduler.Config{Capacity: 1, DefaultMaxQueued: 2, MaxWait: 5 * time.Second})
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 4096, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched})
	front := httptest.NewServer(h)
	defer front.Close()
	call := func(ctx context.Context) <-chan int {
		ch := make(chan int, 1)
		go func() {
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
			addGatewayKey(req, key)
			resp, e := http.DefaultClient.Do(req)
			if e != nil {
				ch <- 0
				return
			}
			defer resp.Body.Close()
			ch <- resp.StatusCode
		}()
		return ch
	}
	first := call(context.Background())
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	queued := call(ctx)
	waitForScheduler(t, func() bool { return sched.Snapshot().Queued == 1 })
	cancel()
	select {
	case <-queued:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled queued request remained blocked")
	}
	waitForScheduler(t, func() bool { return sched.Snapshot().Queued == 0 })
	if s := sched.Snapshot(); s.Active != 1 || s.Queued != 0 {
		t.Fatalf("snapshot after cancellation: %+v", s)
	}
	releaseOnce.Do(func() { close(release) })
	<-first
}

func TestSchedulerLeasesCountTokensAndEmbeddings(t *testing.T) {
	for _, path := range []string{"/v1/messages/count_tokens", "/v1/embeddings"} {
		t.Run(path, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				w.WriteHeader(http.StatusNoContent)
			}))
			defer up.Close()
			defer once.Do(func() { close(release) })
			sched, _ := scheduler.New(scheduler.Config{Capacity: 1, MaxWait: time.Second})
			db, resolver, key := gatewayAuth(t, "127.0.0.1")
			h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{Scheduler: sched})
			front := httptest.NewServer(h)
			defer front.Close()
			result := make(chan int, 1)
			go func() {
				req, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{"model":"m"}`))
				addGatewayKey(req, key)
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					result <- 0
					return
				}
				defer resp.Body.Close()
				result <- resp.StatusCode
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream was not reached")
			}
			if s := sched.Snapshot(); s.Active != 1 {
				t.Fatalf("upstream blocked without active lease: %+v", s)
			}
			once.Do(func() { close(release) })
			if code := <-result; code != http.StatusNoContent {
				t.Fatalf("status %d", code)
			}
		})
	}
}

func TestEnabledInferenceWithoutSchedulerFailsClosed(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer up.Close()
	db, resolver, key := gatewayAuth(t, "127.0.0.1")
	h := NewWithSchedulerConfig(testFiles, db, GatewayConfig{Enabled: true, BaseURL: up.URL, MaxBodyBytes: 1024, SpoolBudgetBytes: 1024, SpoolDir: t.TempDir()}, AuthConfig{Resolver: resolver}, SchedulerConfig{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	addGatewayKey(req, key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code < 500 || hits.Load() != 0 {
		t.Fatalf("status=%d upstream hits=%d; expected fail-closed", w.Code, hits.Load())
	}
}
