package server

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Idriz-hax/model-stead/internal/auth"
	"github.com/Idriz-hax/model-stead/internal/history"
	"github.com/Idriz-hax/model-stead/internal/observability"
	"github.com/Idriz-hax/model-stead/internal/scheduler"
	"sync"
	"sync/atomic"
)

type GatewayConfig struct {
	Enabled                        bool
	BaseURL, APIKey, SpoolDir      string
	RuntimeEnabled                 bool
	RuntimeBaseURL, RuntimeAPIKey  string
	MaxBodyBytes, SpoolBudgetBytes int64
}

type AuthConfig struct {
	PublicOrigin string
	Resolver     *auth.Resolver
	SessionTTL   time.Duration
}

type SchedulerConfig struct {
	Scheduler         *scheduler.Scheduler
	CancellationGrace time.Duration
	History           *history.Store
}

type lifecycleKey struct{}
type metricsOutcomeKey struct{}
type metricsOutcome struct {
	mu     sync.Mutex
	status int
	failed bool
}

func (m *metricsOutcome) setStatus(status int) { m.mu.Lock(); m.status = status; m.mu.Unlock() }
func (m *metricsOutcome) fail()                { m.mu.Lock(); m.failed = true; m.mu.Unlock() }
func (m *metricsOutcome) result() (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, m.failed
}

type lifecycleRecorder struct {
	mu                                                                  sync.Mutex
	store                                                               *history.Store
	userID, requestID, endpoint                                         string
	status                                                              *int
	usage                                                               TokenUsage
	readDone, readError, transportError, proxyReturned, requestCanceled bool
}

func (l *lifecycleRecorder) update(v history.Lifecycle) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.store.UpdateLifecycle(ctx, l.userID, l.requestID, v); err != nil {
		log.Print("request history update failed")
	}
}
func (l *lifecycleRecorder) snapshot() history.Lifecycle {
	l.mu.Lock()
	defer l.mu.Unlock()
	outcome := "upstream_error"
	if l.requestCanceled {
		outcome = "cancelled"
	} else if l.transportError || l.readError {
		outcome = "upstream_error"
	} else if l.proxyReturned && l.readDone && l.status != nil && *l.status >= 400 {
		outcome = "upstream_error"
	} else if l.proxyReturned && l.readDone && l.status != nil {
		outcome = "completed"
	} else {
		outcome = "interrupted"
	}
	completed := time.Now().UTC()
	v := history.Lifecycle{Outcome: outcome, CompletedAt: &completed, ResponseStatus: l.status, UsageStatus: "unavailable"}
	if l.usage.Status != "" {
		v.UsageStatus = l.usage.Status
		v.InputTokens = l.usage.Input
		v.OutputTokens = l.usage.Output
		v.TotalTokens = l.usage.Total
	}
	return v
}

type observedBody struct {
	io.ReadCloser
	recorder *lifecycleRecorder
	ctx      context.Context
	json     *jsonUsageObserver
	sse      *usageObserver
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.recorder.mu.Lock()
		b.recorder.readDone = errors.Is(err, io.EOF)
		if !b.recorder.readDone {
			if b.ctx != nil && b.ctx.Err() != nil {
				b.recorder.requestCanceled = true
			} else {
				b.recorder.readError = true
			}
		}
		if b.json != nil {
			if b.recorder.readDone {
				b.recorder.usage = b.json.Finish()
			} else {
				b.recorder.usage = unavailable()
			}
		}
		if b.sse != nil {
			b.recorder.usage = b.sse.Result()
		}
		b.recorder.mu.Unlock()
	}
	return n, err
}

var spoolUsed atomic.Int64

type spoolBody struct {
	*os.File
	size       int64
	name       string
	privateDir string
	once       sync.Once
	err        error
}

func (b *spoolBody) Close() error {
	b.once.Do(func() {
		b.err = b.File.Close()
		if err := os.Remove(b.name); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.err = errors.Join(b.err, err)
			return
		}
		if b.privateDir != "" {
			if err := os.Remove(b.privateDir); err != nil && !errors.Is(err, os.ErrNotExist) {
				b.err = errors.Join(b.err, err)
			}
		}
		spoolUsed.Add(-b.size)
	})
	return b.err
}

func New(static embed.FS) http.Handler { return NewWithGateway(static, GatewayConfig{}) }

func NewWithGateway(static embed.FS, cfg GatewayConfig) http.Handler {
	return NewWithConfig(static, nil, cfg, AuthConfig{})
}

func NewWithConfig(static embed.FS, db *sql.DB, cfg GatewayConfig, ac AuthConfig) http.Handler {
	var sc SchedulerConfig
	if cfg.Enabled {
		sc.Scheduler, _ = scheduler.New(scheduler.Config{Capacity: 4, DefaultWeight: 1, DefaultMaxQueued: 100, DefaultMaxActiveWhenContended: 2, GlobalMaxQueued: 500, MaxWait: 30 * time.Minute, WorkConserving: true, CancellationGrace: 30 * time.Second})
		sc.CancellationGrace = 30 * time.Second
	}
	return NewWithSchedulerConfig(static, db, cfg, ac, sc)
}

func NewWithSchedulerConfig(static embed.FS, db *sql.DB, cfg GatewayConfig, ac AuthConfig, sc SchedulerConfig) http.Handler {
	mux := http.NewServeMux()
	metrics := observability.New(time.Now(), cfg.Enabled)
	requests := newRequestRegistry()
	check := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
	mux.HandleFunc("GET /health", check)
	mux.HandleFunc("GET /ready", check)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if ac.Resolver == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		identity, err := ac.Resolver.Resolve(r.Context(), r.Header, r.RemoteAddr)
		if err != nil || identity.AuthMethod != "api_key" || identity.Role != "admin" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		if sc.Scheduler == nil {
			_, _ = io.WriteString(w, "scheduler_capacity 0\nscheduler_active 0\nscheduler_reservations 0\nscheduler_queued 0\nscheduler_dispatch_total 0\n")
		} else {
			s := sc.Scheduler.Snapshot()
			_, _ = fmt.Fprintf(w, "scheduler_capacity %d\nscheduler_active %d\nscheduler_reservations %d\nscheduler_queued %d\nscheduler_dispatch_total %d\n", s.Capacity, s.Active, s.Reservations, s.Queued, s.DispatchTotal)
		}
		_, _ = io.WriteString(w, metrics.Prometheus())
	})
	if db != nil {
		registerAuth(mux, db, ac)
		registerAdmin(mux, db, ac, sc.Scheduler, requests, metrics, cfg)
		if sc.History != nil {
			registerUserDashboard(mux, db, ac, sc.History, requests)
		}
	}
	if cfg.Enabled {
		if ac.Resolver == nil {
			panic("gateway requires auth resolver")
		}
		if target, err := url.Parse(cfg.BaseURL); err == nil && (target.Scheme == "http" || target.Scheme == "https") && target.Host != "" && target.User == nil && target.Fragment == "" {
			proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = joinPath(target.Path, pr.In.URL.Path)
				pr.Out.URL.RawPath = ""
				pr.Out.URL.RawQuery = pr.In.URL.RawQuery
				for _, h := range []string{"Authorization", "Proxy-Authorization", "X-Api-Key", "Cookie", "Proxy-Authenticate"} {
					pr.Out.Header.Del(h)
				}
				if cfg.APIKey != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+cfg.APIKey)
				}
			}, FlushInterval: -1, Transport: &http.Transport{DisableCompression: true},
				ModifyResponse: func(resp *http.Response) error {
					if m, ok := resp.Request.Context().Value(metricsOutcomeKey{}).(*metricsOutcome); ok {
						m.setStatus(resp.StatusCode)
					}
					l, _ := resp.Request.Context().Value(lifecycleKey{}).(*lifecycleRecorder)
					if l == nil {
						return nil
					}
					l.mu.Lock()
					code := resp.StatusCode
					l.status = &code
					l.mu.Unlock()
					ct := strings.ToLower(resp.Header.Get("Content-Type"))
					enc := resp.Header.Get("Content-Encoding")
					b := &observedBody{ReadCloser: resp.Body, recorder: l, ctx: resp.Request.Context()}
					if strings.Contains(ct, "text/event-stream") {
						b.sse = NewUsageObserver(l.endpoint, enc)
					} else if strings.Contains(ct, "application/json") {
						b.json = newJSONUsageObserver(l.endpoint)
						if enc != "" && enc != "identity" {
							b.json.failed = true
						}
					}
					b.ReadCloser = NewUsageReadCloser(resp.Body, func(p []byte) {
						if b.json != nil {
							b.json.Feed(p)
						}
						if b.sse != nil {
							b.sse.Feed(p)
						}
					})
					resp.Body = b
					return nil
				}, ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
					if m, ok := r.Context().Value(metricsOutcomeKey{}).(*metricsOutcome); ok {
						m.fail()
					}
					if l, ok := r.Context().Value(lifecycleKey{}).(*lifecycleRecorder); ok {
						l.mu.Lock()
						if r.Context().Err() != nil {
							l.requestCanceled = true
						} else {
							l.transportError = true
						}
						l.mu.Unlock()
					}
					http.Error(w, "Bad Gateway", http.StatusBadGateway)
				}}
			for _, route := range []string{"/v1/models", "/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens", "/v1/embeddings"} {
				path := route
				mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
					allowed := (path == "/v1/models" && r.Method == "GET") || (path != "/v1/models" && r.Method == "POST")
					if !allowed {
						w.Header().Set("Allow", map[bool]string{true: "GET", false: "POST"}[path == "/v1/models"])
						http.Error(w, "method not allowed", 405)
						return
					}
					instrument := r.Method == http.MethodPost && path != "/v1/messages/count_tokens"
					started := time.Now()
					outcome := "interrupted"
					var queueWait time.Duration
					queued := false
					var lifecycle *lifecycleRecorder
					if instrument {
						defer func() { metrics.Record(path, outcome, time.Since(started), queueWait, queued, time.Now()) }()
					}
					metricResult := &metricsOutcome{}
					if instrument {
						r = r.WithContext(context.WithValue(r.Context(), metricsOutcomeKey{}, metricResult))
					}
					identity, err := ac.Resolver.Resolve(r.Context(), r.Header, r.RemoteAddr)
					if err != nil {
						status := http.StatusServiceUnavailable
						var authErr *auth.AuthError
						if errors.As(err, &authErr) && authErr.Kind == auth.Unauthorized {
							status = http.StatusUnauthorized
						} else if auth.IsCapacity(err) {
							status = http.StatusTooManyRequests
						}
						outcome = "rejected"
						http.Error(w, http.StatusText(status), status)
						return
					}
					r = r.WithContext(auth.WithIdentity(r.Context(), identity))
					if path == "/v1/models" && r.Body != nil && r.Body != http.NoBody {
						_ = r.Body.Close()
						http.Error(w, "GET /v1/models does not accept a request body", http.StatusBadRequest)
						return
					}
					if path != "/v1/models" {
						if sc.Scheduler == nil {
							outcome = "rejected"
							http.Error(w, "scheduler unavailable", http.StatusServiceUnavailable)
							return
						}
						model, err := spoolRequest(w, r, cfg)
						if err != nil {
							outcome = "rejected"
							http.Error(w, err.Error(), status(err))
							return
						}
						defer r.Body.Close()
						identityID := identity.SchedulingID
						if identityID == "" {
							identityID = identity.UserID
						}
						request := scheduler.Request{ID: uuid.NewString(), IdentityID: identityID, Endpoint: path}
						if db != nil && identity.UserID != "" {
							if err := db.QueryRowContext(r.Context(), `SELECT scheduling_weight, max_queued, max_active_contended FROM users WHERE id=? AND status='active'`, identity.UserID).Scan(&request.Weight, &request.MaxQueued, &request.MaxActiveWhenContended); err != nil {
								outcome = "rejected"
								http.Error(w, "scheduler policy unavailable", http.StatusServiceUnavailable)
								return
							}
						}
						if sc.History != nil && identity.UserID != "" {
							var requestedModel *string
							validModel := len(model) >= 1 && len(model) <= 128
							for _, char := range model {
								if unicode.IsControl(char) {
									validModel = false
									break
								}
							}
							if validModel {
								requestedModel = &model
							}
							enqueued := time.Now().UTC()
							lifecycle = &lifecycleRecorder{store: sc.History, userID: identity.UserID, requestID: request.ID, endpoint: path, usage: unavailable()}
							if e := sc.History.Create(r.Context(), history.Record{RequestID: request.ID, UserID: identity.UserID, Endpoint: path, RequestedModel: requestedModel, EnqueuedAt: enqueued, Outcome: "queued", UsageStatus: "unavailable"}); e != nil {
								log.Print("request history create failed")
								lifecycle = nil
							}
						}
						requestCtx, cancelRequest := context.WithCancel(r.Context())
						registered := requests.registerIf(liveRequest{ID: request.ID, UserID: identity.UserID, SchedulingID: identityID, Endpoint: path, cancel: cancelRequest}, func() bool {
							if db == nil {
								return true
							}
							var userStatus string
							return db.QueryRowContext(r.Context(), `SELECT status FROM users WHERE id=?`, identity.UserID).Scan(&userStatus) == nil && userStatus == "active"
						})
						if !registered {
							outcome = "rejected"
							cancelRequest()
							if lifecycle != nil {
								done := time.Now().UTC()
								lifecycle.update(history.Lifecycle{Outcome: "rejected", CompletedAt: &done})
							}
							http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
							return
						}
						defer func() { requests.finish(request.ID); cancelRequest() }()
						queueStarted := time.Now()
						lease, err := sc.Scheduler.Submit(requestCtx, request)
						queueWait, queued = time.Since(queueStarted), true
						if err != nil {
							outcome = "rejected"
							if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, scheduler.ErrQueueTimeout) {
								outcome = "timed_out"
							} else if errors.Is(err, context.Canceled) {
								outcome = "cancelled"
							}
							if lifecycle != nil {
								outcome := "rejected"
								if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, scheduler.ErrQueueTimeout) {
									outcome = "timed_out"
								} else if errors.Is(err, context.Canceled) {
									outcome = "cancelled"
								}
								done := time.Now().UTC()
								lifecycle.update(history.Lifecycle{Outcome: outcome, CompletedAt: &done})
							}
							if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
								return
							}
							status := http.StatusServiceUnavailable
							if errors.Is(err, scheduler.ErrIdentityQueueFull) || errors.Is(err, scheduler.ErrGlobalQueueFull) || errors.Is(err, scheduler.ErrQueueTimeout) {
								status = http.StatusTooManyRequests
							}
							http.Error(w, http.StatusText(status), status)
							return
						}
						if requestCtx.Err() != nil {
							outcome = "cancelled"
							lease.Release()
							if lifecycle != nil {
								done := time.Now().UTC()
								lifecycle.update(history.Lifecycle{Outcome: "cancelled", CompletedAt: &done})
							}
							return
						}
						if !requests.activate(request.ID) {
							outcome = "cancelled"
							lease.Release()
							if lifecycle != nil {
								done := time.Now().UTC()
								lifecycle.update(history.Lifecycle{Outcome: "cancelled", CompletedAt: &done})
							}
							return
						}
						if lifecycle != nil {
							started := time.Now().UTC()
							lifecycle.update(history.Lifecycle{Outcome: "active", StartedAt: &started})
						}
						lease.MarkDispatched()
						defer func() {
							if requestCtx.Err() != nil {
								lease.ReleaseAfter(sc.CancellationGrace)
							} else {
								lease.Release()
							}
						}()
						if lifecycle != nil {
							r = r.WithContext(context.WithValue(requestCtx, lifecycleKey{}, lifecycle))
							defer func() {
								lifecycle.mu.Lock()
								lifecycle.proxyReturned = true
								lifecycle.requestCanceled = requestCtx.Err() != nil
								lifecycle.mu.Unlock()
								v := lifecycle.snapshot()
								lifecycle.update(v)
							}()
						} else {
							r = r.WithContext(requestCtx)
						}
					}
					proxy.ServeHTTP(w, r)
					if instrument {
						outcome = "completed"
						status, failed := metricResult.result()
						if r.Context().Err() != nil {
							outcome = "cancelled"
						} else if failed || status >= 400 {
							outcome = "upstream_error"
						}
						if lifecycle != nil {
							l := lifecycle.snapshot()
							switch l.Outcome {
							case "cancelled":
								outcome = "cancelled"
							case "upstream_error":
								outcome = "upstream_error"
							case "interrupted":
								outcome = "interrupted"
							}
						}
					}
				})
			}
		}
	}
	files, err := fs.Sub(static, "dist")
	if err == nil {
		assets := http.FileServer(http.FS(files))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
				http.NotFound(w, r)
				return
			}
			assets.ServeHTTP(w, r)
		})
	}
	return mux
}

func joinPath(a, b string) string { return strings.TrimRight(a, "/") + "/" + strings.TrimLeft(b, "/") }
func status(err error) int {
	if errors.Is(err, errTooLarge) {
		return 413
	}
	if errors.Is(err, errBudget) {
		return 429
	}
	if errors.Is(err, errInvalid) {
		return 400
	}
	return 503
}

var errTooLarge = errors.New("request body too large")
var errInvalid = errors.New("invalid request body")
var errBudget = errors.New("temporary spool budget exhausted")

func spoolRequest(w http.ResponseWriter, r *http.Request, c GatewayConfig) (model string, err error) {
	max := c.MaxBodyBytes
	if max <= 0 {
		max = 20 << 20
	}
	budget := c.SpoolBudgetBytes
	if budget <= 0 {
		budget = 256 << 20
	}
	dir := c.SpoolDir
	privateDir := false
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "model-stead-")
		if err != nil {
			return "", err
		}
		privateDir = true
	} else {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return "", err
		}
	}
	f, err := os.CreateTemp(dir, "router-spool-*")
	if err != nil {
		if privateDir {
			_ = os.Remove(dir)
		}
		return "", err
	}
	name := f.Name()
	retained := false
	var reserved int64
	defer func() {
		if !retained {
			spoolUsed.Add(-reserved)
			_ = f.Close()
			_ = os.Remove(name)
			if privateDir {
				_ = os.Remove(dir)
			}
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return "", err
	}
	defer r.Body.Close()
	limited := http.MaxBytesReader(w, r.Body, max+1)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		nr, readErr := limited.Read(buf)
		if nr > 0 {
			if n+int64(nr) > max {
				return "", errTooLarge
			}
			for {
				used := spoolUsed.Load()
				if used+int64(nr) > budget {
					return "", errBudget
				}
				if spoolUsed.CompareAndSwap(used, used+int64(nr)) {
					reserved += int64(nr)
					break
				}
			}
			nw, writeErr := f.Write(buf[:nr])
			if nw < nr {
				release := int64(nr - nw)
				spoolUsed.Add(-release)
				reserved -= release
			}
			n += int64(nw)
			if writeErr != nil {
				return "", writeErr
			}
			if nw != nr {
				return "", io.ErrShortWrite
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			var maxErr *http.MaxBytesError
			if errors.As(readErr, &maxErr) {
				return "", errTooLarge
			}
			return "", readErr
		}
	}
	var body struct {
		Model string `json:"model"`
	}
	if _, err = f.Seek(0, 0); err != nil {
		f.Close()
		return "", err
	}
	dec := json.NewDecoder(f)
	if dec.Decode(&body) != nil || strings.TrimSpace(body.Model) == "" {
		f.Close()
		return "", errInvalid
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		f.Close()
		return "", errInvalid
	}
	if _, err = f.Seek(0, 0); err != nil {
		return "", err
	}
	retained = true
	bodyFile := &spoolBody{File: f, size: n, name: name}
	if privateDir {
		bodyFile.privateDir = dir
	}
	r.Body = bodyFile
	r.ContentLength = n
	r.Header.Set("Content-Length", strconv.FormatInt(n, 10))
	return strings.TrimSpace(body.Model), nil
}
