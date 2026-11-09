package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Idriz-hax/model-stead/internal/history"
)

func registerUserDashboard(m *http.ServeMux, db *sql.DB, ac AuthConfig, store *history.Store, requests *requestRegistry) {
	write := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, code int) {
		write(w, code, map[string]string{"error": strings.ToLower(http.StatusText(code))})
	}
	guard := func(mut bool, fn func(http.ResponseWriter, *http.Request, sessionPrincipal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, c := lookupSessionPrincipal(r, db)
			if c != 0 {
				fail(w, c)
				return
			}
			if mut && !sessionCSRFValid(r, ac, p) {
				fail(w, 403)
				return
			}
			fn(w, r, p)
		}
	}
	parseDates := func(r *http.Request) (time.Time, time.Time, bool) {
		now := time.Now().UTC()
		from, to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -29), now
		for key, dst := range map[string]*time.Time{"from": &from, "to": &to} {
			if v := r.URL.Query().Get(key); v != "" {
				x, e := time.Parse(time.RFC3339, v)
				if e != nil {
					return from, to, false
				}
				*dst = x.UTC()
			}
		}
		oldest := now.Add(-30 * 24 * time.Hour)
		return from, to, to.After(from) && to.Sub(from) <= 30*24*time.Hour && !from.Before(oldest) && !to.Before(oldest) && !from.After(now) && !to.After(now)
	}
	m.HandleFunc("GET /api/v1/me/usage", guard(false, func(w http.ResponseWriter, r *http.Request, p sessionPrincipal) {
		from, to, ok := parseDates(r)
		if !ok {
			fail(w, 400)
			return
		}
		days, e := store.Usage(r.Context(), p.userID, from, to)
		if e != nil {
			fail(w, 503)
			return
		}
		var input, output *int64
		var count, incomplete int64
		for _, d := range days {
			if d.InputTokens != nil {
				if input == nil {
					v := *d.InputTokens
					input = &v
				} else {
					*input += *d.InputTokens
				}
			}
			if d.OutputTokens != nil {
				if output == nil {
					v := *d.OutputTokens
					output = &v
				} else {
					*output += *d.OutputTokens
				}
			}
			count += d.Requests
			incomplete += d.Incomplete
		}
		write(w, 200, map[string]any{"from": from.Format(time.RFC3339Nano), "to": to.Format(time.RFC3339Nano), "input_tokens": input, "output_tokens": output, "requests": count, "incomplete_requests": incomplete, "days": days})
	}))
	m.HandleFunc("GET /api/v1/me/requests", guard(false, func(w http.ResponseWriter, r *http.Request, p sessionPrincipal) {
		q := history.Query{Limit: 50}
		var e error
		if v := r.URL.Query().Get("limit"); v != "" {
			q.Limit, e = strconv.Atoi(v)
			if e != nil {
				fail(w, 400)
				return
			}
		}
		if v := r.URL.Query().Get("offset"); v != "" {
			q.Offset, e = strconv.Atoi(v)
			if e != nil {
				fail(w, 400)
				return
			}
		}
		q.Endpoint = r.URL.Query().Get("endpoint")
		q.Outcome = r.URL.Query().Get("outcome")
		q.Search = r.URL.Query().Get("search")
		if !utf8.ValidString(q.Search) || utf8.RuneCountInString(q.Search) > 128 || len(q.Search) > 128 {
			fail(w, 400)
			return
		}
		if v := r.URL.Query().Get("from"); v != "" {
			x, err := time.Parse(time.RFC3339, v)
			if err != nil {
				fail(w, 400)
				return
			}
			q.From = &x
		}
		if v := r.URL.Query().Get("to"); v != "" {
			x, err := time.Parse(time.RFC3339, v)
			if err != nil {
				fail(w, 400)
				return
			}
			q.To = &x
		}
		if !validDashboardQuery(q, time.Now().UTC()) {
			fail(w, 400)
			return
		}
		rows, total, e := store.ListPage(r.Context(), p.userID, q)
		if e != nil {
			fail(w, 503)
			return
		}
		live := make(map[string]string)
		requests.mu.Lock()
		for _, v := range requests.entries {
			if v.UserID == p.userID && (v.State == "queued" || v.State == "active") {
				live[v.ID] = v.State
			}
		}
		requests.mu.Unlock()
		items := make([]map[string]any, 0, len(rows))
		for _, v := range rows {
			outcome := v.Outcome
			if state := live[v.RequestID]; state != "" {
				outcome = state
			}
			items = append(items, map[string]any{"request_id": v.RequestID, "endpoint": v.Endpoint, "requested_model": v.RequestedModel, "enqueued_at": v.EnqueuedAt.UTC().Format(time.RFC3339Nano), "started_at": formatTime(v.StartedAt), "completed_at": formatTime(v.CompletedAt), "outcome": outcome, "response_status": v.ResponseStatus, "input_tokens": v.InputTokens, "output_tokens": v.OutputTokens, "total_tokens": v.TotalTokens, "usage_status": v.UsageStatus})
		}
		write(w, 200, map[string]any{"items": items, "limit": q.Limit, "offset": q.Offset, "total": total})
	}))
	m.HandleFunc("POST /api/v1/me/requests/{id}/cancel", guard(true, func(w http.ResponseWriter, r *http.Request, p sessionPrincipal) {
		if !requests.cancelUserRequest(p.userID, r.PathValue("id")) {
			fail(w, 404)
			return
		}
		write(w, 200, map[string]bool{"ok": true})
	}))
}

func validDashboardQuery(q history.Query, now time.Time) bool {
	if q.Limit < 1 || q.Limit > 100 || q.Offset < 0 || len(q.Search) > 128 || !utf8.ValidString(q.Search) || utf8.RuneCountInString(q.Search) > 128 {
		return false
	}
	if q.Endpoint != "" && !dashboardEndpoints[q.Endpoint] || q.Outcome != "" && !dashboardOutcomes[q.Outcome] {
		return false
	}
	oldest := now.Add(-30 * 24 * time.Hour)
	if q.From != nil && (q.From.Before(oldest) || q.From.After(now)) || q.To != nil && (q.To.Before(oldest) || q.To.After(now)) {
		return false
	}
	return q.From == nil || q.To == nil || q.To.After(*q.From) && q.To.Sub(*q.From) <= 30*24*time.Hour
}

var dashboardEndpoints = map[string]bool{"/v1/chat/completions": true, "/v1/completions": true, "/v1/responses": true, "/v1/messages": true, "/v1/embeddings": true}
var dashboardOutcomes = map[string]bool{"queued": true, "active": true, "completed": true, "rejected": true, "timed_out": true, "cancelled": true, "upstream_error": true, "interrupted": true}

func formatTime(v *time.Time) any {
	if v == nil {
		return nil
	}
	return v.UTC().Format(time.RFC3339Nano)
}
