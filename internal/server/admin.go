package server

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Idriz-hax/model-stead/internal/admin"
	"github.com/Idriz-hax/model-stead/internal/observability"
	"github.com/Idriz-hax/model-stead/internal/scheduler"
)

func registerAdmin(m *http.ServeMux, db *sql.DB, ac AuthConfig, sched *scheduler.Scheduler, requests *requestRegistry, metrics *observability.Collector, runtimeCfg ...GatewayConfig) {
	var runtime GatewayConfig
	if len(runtimeCfg) > 0 {
		runtime = runtimeCfg[0]
	}
	store := admin.Store{DB: db}
	var schedulerSettingsMu sync.Mutex
	type principal = sessionPrincipal
	lookup := func(r *http.Request) (principal, int) {
		p, code := lookupSessionPrincipal(r, db)
		if code != 0 {
			return principal{}, code
		}
		if p.role != "admin" || p.forced {
			return principal{}, 403
		}
		return p, 0
	}
	write := func(w http.ResponseWriter, status int, value any) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(value)
	}
	fail := func(w http.ResponseWriter, code int) {
		message := http.StatusText(code)
		write(w, code, map[string]string{"error": strings.ToLower(message)})
	}
	guard := func(mutation bool, fn func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, code := lookup(r)
			if code != 0 {
				fail(w, code)
				return
			}
			if mutation && !sessionCSRFValid(r, ac, p) {
				fail(w, 403)
				return
			}
			fn(w, r, p)
		}
	}
	decode := func(w http.ResponseWriter, r *http.Request, out any) bool {
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if d.Decode(out) != nil {
			fail(w, 400)
			return false
		}
		var x any
		if d.Decode(&x) != io.EOF {
			fail(w, 400)
			return false
		}
		return true
	}
	page := func(r *http.Request) (int, int, bool) {
		limit := 50
		offset := 0
		var e error
		if v := r.URL.Query().Get("limit"); v != "" {
			limit, e = strconv.Atoi(v)
			if e != nil || limit < 1 || limit > 100 {
				return 0, 0, false
			}
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			offset, e = strconv.Atoi(v)
			if e != nil || offset < 0 {
				return 0, 0, false
			}
		}
		return limit, offset, true
	}
	serverError := func(w http.ResponseWriter, err error) {
		if err != nil {
			fail(w, 500)
		}
	}
	m.HandleFunc("GET /api/v1/admin/runtime", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		if !runtime.RuntimeEnabled {
			write(w, 200, runtimeStatus{Enabled: false, Status: "disabled", Configured: []string{}, Running: []string{}, SampledAt: time.Now().UTC()})
			return
		}
		write(w, 200, runtimeSnapshot(r.Context(), runtime.RuntimeBaseURL, runtime.RuntimeAPIKey))
	}))
	m.HandleFunc("GET /api/v1/admin/overview", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		if sched == nil {
			fail(w, 503)
			return
		}
		schedulerData := schedulerSnapshot(sched.Snapshot())
		schedulerData["oldest_queued_wait_seconds"] = oldestQueuedWaitSeconds(requests)
		write(w, 200, map[string]any{"scheduler": schedulerData, "requests": requestSnapshot(requests), "observability": metrics.Snapshot(time.Now())})
	}))
	m.HandleFunc("GET /api/v1/admin/requests", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		limit, offset, ok := page(r)
		if !ok {
			fail(w, 400)
			return
		}
		items := requestSnapshot(requests)
		total := len(items)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}
		write(w, 200, map[string]any{"items": items[offset:end], "limit": limit, "offset": offset, "total": total})
	}))
	m.HandleFunc("POST /api/v1/admin/requests/{id}/cancel", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		id := r.PathValue("id")
		found := false
		for _, item := range requestSnapshot(requests) {
			if item["id"] == id {
				found = true
				break
			}
		}
		if !found {
			fail(w, 404)
			return
		}
		_, e := db.ExecContext(r.Context(), `INSERT INTO audit_events(actor_id,action,target_type,target_id,before_json,after_json) VALUES(?, 'request.cancel','request',?,'{}','{}')`, p.userID, id)
		if e != nil {
			fail(w, 500)
			return
		}
		// If the request finished after the existence check, cancellation is already complete.
		requests.cancelRequest(id)
		write(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("GET /api/v1/admin/users", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		l, o, ok := page(r)
		if !ok {
			fail(w, 400)
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("search"))
		if len(q) > 128 {
			fail(w, 400)
			return
		}
		v, total, e := store.ListUsersPage(r.Context(), q, l, o)
		if e != nil {
			serverError(w, e)
			return
		}
		counts := requestCountsByUser(requests)
		items := make([]map[string]any, 0, len(v))
		for _, user := range v {
			count := counts[user.ID]
			items = append(items, map[string]any{"id": user.ID, "username": user.Username, "display_name": user.DisplayName, "role": user.Role, "status": user.Status, "scheduling_weight": user.SchedulingWeight, "max_queued": user.MaxQueued, "max_active_contended": user.MaxActiveContended, "must_change_password": user.MustChangePassword, "queued_requests": count[0], "active_requests": count[1]})
		}
		write(w, 200, map[string]any{"items": items, "limit": l, "offset": o, "total": total})
	}))
	m.HandleFunc("POST /api/v1/admin/users", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		var in struct {
			admin.User
			TemporaryPassword string `json:"temporary_password"`
		}
		if !decode(w, r, &in) {
			return
		}
		v, e := store.CreateUser(r.Context(), p.userID, in.User, in.TemporaryPassword)
		if e != nil {
			fail(w, 400)
			return
		}
		write(w, 201, v)
	}))
	m.HandleFunc("GET /api/v1/admin/users/{id}", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		v, e := store.GetUser(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, 404)
			return
		}
		write(w, 200, v)
	}))
	m.HandleFunc("PATCH /api/v1/admin/users/{id}", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		var v admin.User
		if !decode(w, r, &v) {
			return
		}
		v.ID = r.PathValue("id")
		if e := store.UpdateUser(r.Context(), p.userID, v); e != nil {
			fail(w, 400)
			return
		}
		write(w, 200, v)
	}))
	for _, action := range []string{"disable", "enable"} {
		action := action
		m.HandleFunc("POST /api/v1/admin/users/{id}/"+action, guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
			id := r.PathValue("id")
			var e error
			if action == "disable" {
				e = store.DisableUser(r.Context(), p.userID, id)
				if e == nil {
					requests.disableUser(id)
				}
			} else {
				e = store.EnableUser(r.Context(), p.userID, id)
			}
			if e != nil {
				fail(w, 400)
				return
			}
			write(w, 200, map[string]bool{"ok": true})
		}))
	}
	m.HandleFunc("POST /api/v1/admin/users/{id}/reset-password", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		var in struct {
			TemporaryPassword string `json:"temporary_password"`
		}
		if !decode(w, r, &in) {
			return
		}
		if e := store.ResetTemporaryPassword(r.Context(), p.userID, r.PathValue("id"), in.TemporaryPassword); e != nil {
			fail(w, 400)
			return
		}
		write(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("GET /api/v1/admin/devices", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		l, o, ok := page(r)
		if !ok {
			fail(w, 400)
			return
		}
		v, e := store.ListDevicesPage(r.Context(), l, o)
		if e != nil {
			serverError(w, e)
			return
		}
		write(w, 200, map[string]any{"items": v, "limit": l, "offset": o})
	}))
	m.HandleFunc("POST /api/v1/admin/devices", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		var in struct {
			UserID    string `json:"user_id"`
			Label     string `json:"label"`
			IPAddress string `json:"ip_address"`
		}
		if !decode(w, r, &in) {
			return
		}
		v, e := store.CreateDevice(r.Context(), p.userID, in.UserID, in.Label, in.IPAddress)
		if e != nil {
			fail(w, 400)
			return
		}
		write(w, 201, v)
	}))
	m.HandleFunc("GET /api/v1/admin/devices/{id}", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		v, e := store.GetDevice(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, 404)
			return
		}
		write(w, 200, v)
	}))
	m.HandleFunc("PATCH /api/v1/admin/devices/{id}", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		var in struct {
			UserID    string `json:"user_id"`
			Label     string `json:"label"`
			IPAddress string `json:"ip_address"`
		}
		if !decode(w, r, &in) {
			return
		}
		id := r.PathValue("id")
		if e := store.UpdateDevice(r.Context(), p.userID, id, in.UserID, in.Label, in.IPAddress); e != nil {
			fail(w, 400)
			return
		}
		v, e := store.GetDevice(r.Context(), id)
		if e != nil {
			fail(w, 500)
			return
		}
		write(w, 200, v)
	}))
	for _, action := range []string{"trust", "untrust"} {
		action := action
		m.HandleFunc("POST /api/v1/admin/devices/{id}/"+action, guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
			var e error
			if action == "trust" {
				e = store.TrustDevice(r.Context(), p.userID, r.PathValue("id"))
			} else {
				e = store.UntrustDevice(r.Context(), p.userID, r.PathValue("id"))
			}
			if e != nil {
				fail(w, 400)
				return
			}
			write(w, 200, map[string]bool{"ok": true})
		}))
	}
	m.HandleFunc("GET /api/v1/admin/api-keys", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		l, o, ok := page(r)
		if !ok {
			fail(w, 400)
			return
		}
		uid := r.URL.Query().Get("user_id")
		v, e := store.ListAPIKeysPage(r.Context(), uid, l, o)
		if e != nil {
			serverError(w, e)
			return
		}
		write(w, 200, map[string]any{"items": v, "limit": l, "offset": o})
	}))
	m.HandleFunc("POST /api/v1/admin/api-keys", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		var in struct {
			UserID    string     `json:"user_id"`
			Name      string     `json:"name"`
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if !decode(w, r, &in) {
			return
		}
		v, secret, e := store.CreateAPIKey(r.Context(), p.userID, in.UserID, in.Name, in.ExpiresAt)
		if e != nil {
			fail(w, 400)
			return
		}
		write(w, 201, map[string]any{"key": v, "secret": secret})
	}))
	m.HandleFunc("POST /api/v1/admin/api-keys/{id}/revoke", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		var owner string
		if e := db.QueryRowContext(r.Context(), `SELECT user_id FROM api_keys WHERE id=?`, r.PathValue("id")).Scan(&owner); e != nil {
			fail(w, 404)
			return
		}
		if e := store.RevokeAPIKey(r.Context(), p.userID, owner, r.PathValue("id")); e != nil {
			fail(w, 400)
			return
		}
		write(w, 200, map[string]bool{"ok": true})
	}))
	m.HandleFunc("GET /api/v1/admin/scheduler", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		v, e := store.Settings(r.Context())
		if e != nil {
			serverError(w, e)
			return
		}
		write(w, 200, v)
	}))
	m.HandleFunc("PUT /api/v1/admin/scheduler", guard(true, func(w http.ResponseWriter, r *http.Request, p principal) {
		if sched == nil {
			fail(w, 503)
			return
		}
		var v admin.SchedulerSettings
		if !decode(w, r, &v) {
			return
		}
		c := scheduler.Config{Capacity: v.GlobalCapacity, DefaultWeight: v.DefaultWeight, DefaultMaxQueued: v.MaxQueued, DefaultMaxActiveWhenContended: v.MaxActiveContended, GlobalMaxQueued: v.GlobalMaxQueued, MaxWait: v.MaxWait, WorkConserving: v.WorkConserving, CancellationGrace: v.CancellationGrace}
		if e := admin.ValidateSettings(v); e != nil {
			fail(w, 400)
			return
		}
		if v.MaxActiveContended > v.GlobalCapacity {
			fail(w, 400)
			return
		}
		schedulerSettingsMu.Lock()
		defer schedulerSettingsMu.Unlock()
		if e := store.UpdateSettings(r.Context(), p.userID, v); e != nil {
			fail(w, 500)
			return
		}
		if e := sched.UpdateConfig(c); e != nil {
			fail(w, 500)
			return
		}
		write(w, 200, v)
	}))
	m.HandleFunc("GET /api/v1/admin/audit", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		l, _, ok := page(r)
		if !ok {
			fail(w, 400)
			return
		}
		cursor := int64(0)
		if x := r.URL.Query().Get("before_id"); x != "" {
			var e error
			cursor, e = strconv.ParseInt(x, 10, 64)
			if e != nil {
				fail(w, 400)
				return
			}
		}
		actor, action, target := r.URL.Query().Get("actor"), r.URL.Query().Get("action"), r.URL.Query().Get("target")
		if len(actor) > 128 || len(action) > 128 || len(target) > 128 {
			fail(w, 400)
			return
		}
		v, e := store.ListAuditFiltered(r.Context(), cursor, l, actor, action, target)
		if e != nil {
			fail(w, 400)
			return
		}
		write(w, 200, map[string]any{"items": v, "before_id": cursor, "limit": l})
	}))
	m.HandleFunc("GET /api/v1/admin/events", guard(false, func(w http.ResponseWriter, r *http.Request, _ principal) {
		if sched == nil {
			fail(w, 503)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			fail(w, 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		schedulerChanges, stopScheduler := sched.WatchChanges()
		defer stopScheduler()
		requestChanges, stopRequests := requests.WatchChanges()
		defer stopRequests()
		send := func() {
			b, _ := json.Marshal(map[string]any{"scheduler": schedulerSnapshot(sched.Snapshot()), "requests": requestSnapshot(requests)})
			_, _ = w.Write(append(append([]byte("event: snapshot\ndata: "), b...), []byte("\n\n")...))
			flusher.Flush()
		}
		// Subscribe first, then snapshot, so concurrent updates are either in the snapshot or queued for replay.
		send()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case _, ok := <-schedulerChanges:
				if !ok {
					return
				}
				send()
			case _, ok := <-requestChanges:
				if !ok {
					return
				}
				send()
			case <-ticker.C:
				_, _ = w.Write([]byte(": heartbeat\n\n"))
				flusher.Flush()
			}
		}
	}))
}

func requestSnapshot(r *requestRegistry) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, 0, len(r.entries))
	now := time.Now()
	for _, v := range r.entries {
		item := map[string]any{"id": v.ID, "user_id": v.UserID, "identity_id": v.SchedulingID, "endpoint": v.Endpoint, "state": v.State, "enqueued_at": v.EnqueuedAt}
		until := now
		if !v.StartedAt.IsZero() {
			item["started_at"] = v.StartedAt
			until = v.StartedAt
		}
		item["queue_wait_seconds"] = until.Sub(v.EnqueuedAt).Seconds()
		out = append(out, item)
	}
	return out
}

func requestCountsByUser(r *requestRegistry) map[string][2]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	counts := make(map[string][2]int)
	for _, v := range r.entries {
		count := counts[v.UserID]
		if v.State == "queued" {
			count[0]++
		} else if v.State == "active" {
			count[1]++
		}
		counts[v.UserID] = count
	}
	return counts
}

func oldestQueuedWaitSeconds(r *requestRegistry) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	var oldest time.Duration
	for _, v := range r.entries {
		if v.State == "queued" && now.Sub(v.EnqueuedAt) > oldest {
			oldest = now.Sub(v.EnqueuedAt)
		}
	}
	return oldest.Seconds()
}

func schedulerSnapshot(s scheduler.Snapshot) map[string]any {
	identities := make(map[string]any, len(s.Identities))
	for id, v := range s.Identities {
		identities[id] = map[string]any{"queued": v.Queued, "active": v.Active, "weight": v.Weight}
	}
	requests := make([]map[string]any, 0, len(s.Requests))
	for _, v := range s.Requests {
		requests = append(requests, map[string]any{"id": v.ID, "identity_id": v.IdentityID, "endpoint": v.Endpoint, "state": v.State, "enqueued_at": v.EnqueuedAt})
	}
	return map[string]any{"capacity": s.Capacity, "active": s.Active, "reservations": s.Reservations, "queued": s.Queued, "available": s.Available, "identities": identities, "requests": requests, "dispatch_total": s.DispatchTotal}
}
