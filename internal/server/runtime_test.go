package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRuntimeV269ContractAndFixedPaths(t *testing.T) {
	seen := map[string]bool{}
	var seenMu sync.Mutex
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		if r.Method != http.MethodGet {
			t.Errorf("method=%s", r.Method)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		if r.URL.Path != "/health" && r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("missing bearer for %s", r.URL.Path)
		}
		seenMu.Lock()
		seen[r.URL.Path] = true
		seenMu.Unlock()
		switch r.URL.Path {
		case "/health":
			w.Write([]byte("OK"))
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"alpha"}]}`))
		case "/running":
			w.Write([]byte(`{"running":[{"model":"alpha","state":"running"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	done := make(chan runtimeStatus, 1)
	go func() { done <- runtimeSnapshot(context.Background(), up.URL, "secret") }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("status requests were not concurrent")
		}
	}
	close(release)
	v := <-done
	if v.Status != "reachable" || !v.Health || len(v.Configured) != 1 || len(v.Running) != 1 {
		t.Fatalf("snapshot=%+v", v)
	}
	for _, p := range []string{"/health", "/v1/models", "/running"} {
		seenMu.Lock()
		if !seen[p] {
			t.Errorf("endpoint not called: %s", p)
		}
		seenMu.Unlock()
	}
}

func TestRuntimeRequiredArrays(t *testing.T) {
	for _, tc := range []struct{ path, body string }{{"/v1/models", `{}`}, {"/running", `{}`}} {
		t.Run(tc.path, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					w.Write([]byte("OK"))
				case tc.path:
					w.Write([]byte(tc.body))
				case "/v1/models":
					w.Write([]byte(`{"data":[]}`))
				case "/running":
					w.Write([]byte(`{"running":[]}`))
				}
			}))
			defer up.Close()
			if got := runtimeSnapshot(context.Background(), up.URL, "secret").Status; got != "malformed" {
				t.Fatalf("status=%s", got)
			}
		})
	}
}

func TestRuntimeDisabledSnapshotShape(t *testing.T) {
	v := runtimeStatus{Enabled: false, Status: "disabled", Configured: []string{}, Running: []string{}, SampledAt: time.Now().UTC()}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err = json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"enabled", "status", "healthy", "configured_models", "running_models", "sampled_at"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing field %s", key)
		}
	}
}

func TestRuntimeFailureClassificationAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		code       int
		wait       bool
		want       string
	}{
		{"unauthorized", "", 401, false, "unauthorized"}, {"server", "", 503, false, "unavailable"}, {"malformed", "{", 200, false, "malformed"}, {"empty", `{"data":[]}`, 200, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					w.Write([]byte("OK"))
				case "/v1/models":
					w.WriteHeader(tc.code)
					w.Write([]byte(tc.body))
				case "/running":
					w.Write([]byte(`{"running":[]}`))
				}
			}))
			defer up.Close()
			v := runtimeSnapshot(context.Background(), up.URL, "secret")
			if tc.name == "empty" {
				if v.Status != "reachable" || len(v.Configured) != 0 {
					t.Fatalf("%+v", v)
				}
			} else if v.Status != tc.want {
				t.Fatalf("status=%s want=%s", v.Status, tc.want)
			}
		})
	}
}

func TestRuntimeRedirectOversizeMalformedSecretAndTimeout(t *testing.T) {
	secret := "never-return-secret"
	redirect := httptest.NewServer(http.RedirectHandler("http://127.0.0.1:1/", http.StatusFound))
	defer redirect.Close()
	v := runtimeSnapshot(context.Background(), redirect.URL, secret)
	if v.Status != "unsupported" {
		t.Fatalf("redirect status=%s", v.Status)
	}
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"oversize", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.Write([]byte("OK"))
			} else if r.URL.Path == "/v1/models" {
				w.Write(make([]byte, (1<<20)+2))
			} else {
				w.Write([]byte(`{"running":[]}`))
			}
		}, "malformed"},
		{"timeout", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				time.Sleep(3200 * time.Millisecond)
				w.Write([]byte("OK"))
			} else {
				w.Write([]byte(`{}`))
			}
		}, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(tc.handler)
			defer up.Close()
			v := runtimeSnapshot(context.Background(), up.URL, secret)
			if v.Status != tc.want {
				t.Fatalf("status=%s", v.Status)
			}
		})
	}
	if secret == v.Status {
		t.Fatal("secret leaked")
	}
}
