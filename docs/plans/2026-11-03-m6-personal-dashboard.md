# M6 Personal Dashboard Implementation Plan

> **Archive status:** M6 is implemented. This dated plan records its execution workflow; unchecked boxes are historical checklist state, not current open tasks. See [milestone index](../milestones.md).

**Goal:** Deliver an owner-private usage dashboard, 30-day inference history, provider-reported token counts, and owner-only live request cancellation.

**Architecture:** Persist one bounded row per authenticated account-owned inference request in SQLite. Observe supported upstream JSON/SSE usage while the existing ReverseProxy forwards bytes unchanged. Serve session-authenticated, owner-scoped `/api/v1/me` routes; leave anonymous traffic and M5 admin history out of scope.

**Tech Stack:** Go 1.23, `net/http` `httputil.ReverseProxy`, SQLite/Goose, existing React 19 + TypeScript/Vite, no new dependencies.

---

## Scope guard

Implement only the approved M6 spec: `docs/specs/2026-11-03-m6-personal-dashboard-design.md`. Preserve the gateway-disabled default, llama-swap ownership, M5 admin routes, and exact upstream response bytes/headers/status/streaming. Never store prompts, request/response bodies, credentials, API-key material, or arbitrary headers. Token usage is provider-reported only; no cost or tokenizer estimates. Anonymous/unowned traffic has no personal history. M7/M8 remain deferred. This workspace has no Git metadata; do not create worktrees or attempt commits.

## File map

- `internal/database/migrations/00004_request_history.sql` — forward-only request-history table and owner/time indexes.
- `internal/history/store.go`, `store_test.go` — typed request records, owner-scoped list/aggregates, finalization, startup recovery, and 30-day purge.
- `internal/server/usage.go`, `usage_test.go` — bounded JSON/SSE usage extraction; no response rewriting or full-body buffering.
- `internal/server/requests.go`, `requests_test.go` — owner-checked live lookup/cancel; keep live state in memory.
- `internal/server/server.go`, `user_dashboard.go`, matching tests — record lifecycle, proxy observation, personal session routes, strict owner scoping.
- `internal/server/admin.go` / tests — share session-principal lookup/CSRF helpers without weakening admin checks.
- `cmd/router/main.go` — recover interrupted rows, purge expired history, inject history store and cleanup lifecycle.
- `frontend/src/api.ts`, `AdminApp.tsx`, `style.css` — role-specific nav, balanced dashboard, paged history, owner-only cancellation.
- `README.md` — document M6 privacy, retention, provider-reported usage, and user controls.

## Tasks

### Task 1: History schema and owner-scoped store

**Files:** Create `internal/database/migrations/00004_request_history.sql`; create `internal/history/store.go`, `internal/history/store_test.go`; inspect `internal/database/database_test.go` and `internal/admin/store.go` for migration/store conventions.

- [ ] **Step 1: Write store tests first.** Add SQLite tests for user-owned insert/finalize, cross-user list isolation, nullable/partial token counts, all outcome values, pagination/filter bounds, UTC range aggregation, expired-row purge, and queued/active rows recovered as interrupted. Seed two users and the fixture settings using the existing database test helpers.
- [ ] **Step 2: Run the focused tests to confirm the red state.** Run `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./internal/history -count=1`. Expected: package/store and migration symbols are missing.
- [ ] **Step 3: Add migration and typed store.** Create `request_history` with `request_id TEXT PRIMARY KEY`, `user_id TEXT NOT NULL REFERENCES users(id)`, `endpoint TEXT NOT NULL CHECK(endpoint IN ('/v1/chat/completions','/v1/completions','/v1/responses','/v1/messages','/v1/embeddings'))`, nullable `requested_model`, `enqueued_at_ms INTEGER NOT NULL`, nullable `started_at_ms`/`completed_at_ms`, `outcome TEXT NOT NULL CHECK(outcome IN ('queued','active','completed','rejected','timed_out','cancelled','upstream_error','interrupted'))`, nullable `response_status CHECK(response_status BETWEEN 100 AND 599)`, nullable non-negative `input_tokens`/`output_tokens`/`total_tokens`, and `usage_status TEXT NOT NULL CHECK(usage_status IN ('reported','partial','unavailable'))`. Add `(user_id,enqueued_at_ms DESC,request_id DESC)` and `(user_id,outcome,enqueued_at_ms DESC)` indexes. Store methods take `userID` separately from query filters; every list/aggregate SQL query must include `WHERE user_id=?`. Model nullability explicitly:

```go
type Record struct {
    RequestID, UserID, Endpoint string
    RequestedModel             *string
    EnqueuedAt                  time.Time
    StartedAt, CompletedAt      *time.Time
    Outcome                     string
    ResponseStatus              *int
    InputTokens, OutputTokens   *int64
    TotalTokens                 *int64
    UsageStatus                 string
}
```
- [ ] **Step 4: Implement `Create`, `UpdateLifecycle`, `ListPage`, `Usage`, `RecoverInterrupted`, and `PurgeBefore`.** Use parameterized SQL and bounded `limit` 1–100 (default 50), offset ≥0, inclusive `from`/exclusive `to` date range ≤30 days, valid endpoint/outcome filters, and search ≤128 UTF-8 bytes. Do not accept raw SQL or arbitrary payloads. Store UTC Unix milliseconds; encode API timestamps as RFC3339 UTC. `Usage` returns daily input/output sums, request count, and count of rows whose usage status is `partial` or `unavailable`.
- [ ] **Step 5: Run focused store/database tests.** Run `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./internal/history ./internal/database -count=1`. Expected: all migration, owner-isolation, query, retention, and recovery tests pass.

### Task 2: Bounded provider usage parser

**Files:** Create `internal/server/usage.go` and `internal/server/usage_test.go`; inspect the `httputil.ReverseProxy` construction at `internal/server/server.go:122-134` and current streaming proxy tests.

- [ ] **Step 1: Write parser tests before implementation.** Cover exact JSON paths for chat/completions root `usage.prompt_tokens`/`completion_tokens`, Responses root `usage.input_tokens`/`output_tokens`, Anthropic Messages root `usage.input_tokens`/`output_tokens`, and embeddings root `usage.prompt_tokens`/`total_tokens`. Cover SSE chat usage chunks, Responses `response.completed.response.usage`, Anthropic `message_start.message.usage` and cumulative `message_delta.usage`; assert repeated cumulative snapshots replace, not add. Also cover partial/missing/negative/fraction/string/overflow counts, arbitrary nested `usage` ignored, CR/LF/multiline framing, every-byte chunk splitting, 64-KiB oversize event, unsupported content encoding, and large skipped response strings. Assert parser returns nullable input/output/total plus `reported`, `partial`, or `unavailable`.
- [ ] **Step 2: Run focused tests to confirm they fail.** Run `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./internal/server -run '^TestUsage' -count=1`. Expected: parser types/functions do not exist.
- [ ] **Step 3: Implement a bounded incremental observer.** Define `TokenUsage{Input, Output, Total *int64; Status string}` and `usageObserver.Feed([]byte)`. JSON scanning skips non-usage strings without storing them; cap nesting at 64, retained keys at 64 bytes, captured usage objects at 4 KiB, and numeric tokens at 20 characters. SSE framing supports LF/CRLF/CR, comments, and multiline `data:`; cap the combined event at 64 KiB and disable accounting for that response after overflow. Decode only exact endpoint-specific usage paths, distinguish missing/null from reported zero, replace cumulative snapshots (especially Anthropic `message_delta`) rather than summing them, and reject conflicting/invalid recognized fields as unavailable. Derive total only from a provider-reported total or checked input+output when both exist; otherwise leave it nil.
- [ ] **Step 4: Add the read-through wrapper.** Use `ReverseProxy.ModifyResponse` to wrap only the upstream response body in a `ReadCloser`:

```go
func (b *usageReadCloser) Read(p []byte) (int, error) {
    n, err := b.body.Read(p)
    if n > 0 { b.observer.Feed(p[:n]) }
    return n, err
}
func (b *usageReadCloser) Close() error { return b.body.Close() }
```

Its `Read` calls the original once, feeds only returned bytes, and returns the exact original `n, err`, including `n>0` with EOF/error; `Close` delegates directly without draining. Do not read ahead, batch writes, alter flush policy, or block on persistence. Mark content encodings other than identity unavailable without changing them. Keep `FlushInterval: -1`; bounded inline parsing CPU is permitted, but parser errors never affect forwarding.
- [ ] **Step 5: Verify format/chunk tests and existing proxy tests.** Run `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./internal/server -run 'Test(Usage|Proxy|Gateway)' -count=1`. Expected: every parser case passes and existing proxy byte/flush behavior remains unchanged.

### Task 3: Inference lifecycle capture and registry ownership

**Files:** Modify `internal/server/server.go`, `internal/server/requests.go`, `internal/server/requests_test.go`, `internal/server/server_test.go`; add `History *history.Store` to `SchedulerConfig`; update `cmd/router/main.go` startup wiring.

- [ ] **Step 1: Add request-registry ownership tests.** Test `cancelUserRequest(userID, requestID)` cancels the owner's queued/active entry, rejects another user's ID without canceling it, and returns false for finished requests. Add concurrent cancel/finish coverage and run `go test ./internal/server -run 'Test(RequestRegistry|CancelUserRequest)' -count=1` (expected red).
- [ ] **Step 2: Implement atomic owner-checked cancellation.** Under the registry mutex, find the ID and compare `entry.UserID` with the authenticated owner; capture the cancel function, unlock, then invoke it. Never expose a registry snapshot and authorize in a later step.
- [ ] **Step 3: Return the validated requested model from `spoolRequest`.** Change its signature to `(model string, err error)` and update all seven callers identified by `grep -R 'spoolRequest(' internal/server`. Existing body validation and spool cleanup remain unchanged; add tests that a valid model is returned and invalid JSON still fails.
- [ ] **Step 4: Start/finalize history rows in the inference handler.** After `spoolRequest` succeeds and the resolved `identity.UserID` is nonempty, normalize the model to the 1–128-byte/control-free bound and attempt `History.Create`. On write failure log a generic server-side error and continue. Update outcome/timestamps at queue failure, activation, cancellation, and proxy completion; never pass request bodies to the history store.
- [ ] **Step 5: Observe upstream responses through `ReverseProxy.ModifyResponse`.** Put a request-scoped recorder in context; in `ModifyResponse`, wrap only `resp.Body`, store `resp.StatusCode`, and attach the format-aware observer from Task 2. Track transport/read completion, proxy return, and request cancellation separately. Mark upstream transport failures through `ErrorHandler`; never infer success from EOF alone.
- [ ] **Step 6: Finalize once outside stream copying.** Install one defer around `proxy.ServeHTTP`; persist final outcome/status/tokens after the proxy returns or unwinds, using an independent bounded context (not the canceled inference context). Preserve any `http.ErrAbortHandler` panic; do not recover or write a second response. Ensure deferred cleanup cancellation after a successful response does not relabel it cancelled.
- [ ] **Step 7: Wire the store and test lifecycle.** In `cmd/router/main.go`, create the store after migrations, call interrupted-record recovery and 30-day purge before serving, inject it through `SchedulerConfig`, and run periodic purge. Test user-owned vs unowned requests, queued rejection/timeout, upstream success/error, client cancel, restart recovery, and history-write failure without inference failure.
- [ ] **Step 8: Run focused lifecycle and race tests.** Run `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./internal/server ./internal/history -count=1` and `docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -race ./internal/server ./internal/history -count=1`. Expected: all pass.

### Task 4: Session-authenticated `/api/v1/me` endpoints

**Files:** Create `internal/server/session_guard.go` and `internal/server/user_dashboard.go`; modify `internal/server/admin.go` to share the session principal/CSRF helpers; modify `internal/server/server.go`; add `internal/server/user_dashboard_test.go`.

- [ ] **Step 1: Write route authorization tests first.** Using the existing SQLite/session fixture, cover anonymous, API-key-only, disabled, forced-password-change, user, and admin sessions; wrong Origin/CSRF on cancel; cross-user list/cancel denial; and no-store headers. Run `go test ./internal/server -run '^TestUserDashboard' -count=1` (expected red).
- [ ] **Step 2: Extract a shared active-session principal helper.** Resolve the session cookie with `auth.LookupSession`, reload the active user's ID/role/forced-password flag from SQLite, and return a typed principal. Provide a shared exact-Origin/constant-time CSRF check. Refactor the existing admin guard to require role `admin` and non-forced status; retain its current HTTP status behavior.
- [ ] **Step 3: Implement the three user routes.** `GET /api/v1/me/usage` returns the caller's server-computed 30-day totals/daily buckets; `GET /api/v1/me/requests` returns only `principal.UserID` rows with validated filters/page bounds; `POST /api/v1/me/requests/{id}/cancel` checks CSRF and calls the atomic owner-checked registry method. Wrong-owner, unknown, or finished IDs all return the same 404 response.
- [ ] **Step 4: Overlay live state without duplicating rows.** Read the requested history page for the caller, map only that caller's registry entries by request ID, and replace outcome with current `queued`/`active` while present. Do not return any other owner's ID, metadata, token totals, or counts.
- [ ] **Step 5: Register routes and verify.** Register the user handlers alongside auth/admin only when the DB is configured. Run `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./internal/server ./internal/history -run 'Test(UserDashboard|AdminAuthorization|AdminSSE|RequestRegistry)' -count=1`. Expected: user and existing admin auth tests pass.

### Task 5: Personal dashboard session gate and overview

**Files:** Create `frontend/src/PersonalApp.tsx`; modify `frontend/src/AdminApp.tsx`, `frontend/src/api.ts`, and `frontend/src/style.css`.

- [ ] **Step 1: Add typed personal API helpers.** Reuse the existing same-origin `request<T>` helper, but target `/api/v1/me/usage`, `/api/v1/me/requests`, and `/api/v1/me/requests/{id}/cancel`; never route personal requests through `/api/v1/admin/` helpers. Mutation requests continue to add Origin and CSRF headers.
- [ ] **Step 2: Update the session gate.** Keep anonymous login and forced-password-change routing. On successful password change, clear the revoked session and show a sign-in confirmation; after login, active non-admin users enter `PersonalApp` rather than the M5 denial screen. Add an admin-only “My dashboard” entry that opens the same component using the admin's own session; keep all M5 admin links and data paths admin-only.
- [ ] **Step 3: Build the responsive personal shell.** `PersonalApp` owns only the My dashboard and Request history views, profile/sign-out, load errors, and current page state. Add the selected balanced layout without adding a dependency; leave the admin Shell's existing seven pages and SSE behavior unchanged.
- [ ] **Step 4: Implement overview data and states.** Load `/usage` and recent `/requests`; show 30-day request/input/output totals, partial-or-unavailable count, daily input/output trend, and recent activity. Render a small inline SVG or CSS chart with an accessible text/table alternative. Unknown counts display as “Unavailable”, never `0`.
- [ ] **Step 5: Verify frontend gate and overview.** Run `cd frontend && npm run build`. Browser-smoke user/admin/forced-password/anonymous gates and overview loading, empty, error/retry, chart labels, and mobile layout with mocked `/api/v1/me/*` responses. Expected: users see no admin navigation and admins cannot load another account's personal data.

### Task 6: Request history, filters, pagination, cancellation UI

**Files:** Modify `frontend/src/PersonalApp.tsx` and `frontend/src/style.css`; extend `internal/server/user_dashboard_test.go` as API contracts settle.

- [ ] **Step 1: Add controlled history filters.** Keep date range within the last 30 days; endpoint and outcome use allowlisted selects; model search is limited to 128 characters. Submitting filters resets the page to offset zero and preserves entered values after a load error.
- [ ] **Step 2: Render paginated history.** Fetch `/api/v1/me/requests?limit=50&offset=…` and show requested model, endpoint, outcome/status, enqueue/start/completion, queue wait/duration, and input/output/total tokens. Keep null distinct from zero. Use server `total`; disable Previous/Next at boundaries and show a clear empty state.
- [ ] **Step 3: Merge and refresh live requests.** Render each request ID once; use its live `queued`/`active` state from the server overlay. While any visible row is queued or active, refresh its page every five seconds; stop the timer when no visible live rows remain. Manual refresh remains available.
- [ ] **Step 4: Add owner-only cancellation UX.** Show cancel only for live owned rows. Cancel queued work immediately; for active work call `window.confirm('Cancel this active request? The upstream inference and client response will be interrupted.')`. Disable the row control while pending; on success refresh the page and announce status, on error preserve the row and show an operation error.
- [ ] **Step 5: Smoke responsive and accessible behavior.** Re-run `cd frontend && npm run build`; use browser checks for filters, paging, partial/unavailable usage, queued cancel, active-confirm/cancel, failure/retry, keyboard/focus, and 390px width without page-level horizontal overflow.

### Task 7: Retention, docs, full verification, and reviews

**Files:** Modify `README.md`; inspect `cmd/router/main.go`, `internal/server`, and `frontend` for uncovered spec behavior.

- [ ] **Step 1: Document M6 behavior.** Explain personal session-only access, owner-scoped history, 30-day automatic retention, provider-reported-only tokens (partial/unavailable when fields are absent, unsupported, encoded, malformed, or over parser bounds), cancellation ownership/confirmation, and the explicit absence of prompts, response bodies, cost estimates, and anonymous history.
- [ ] **Step 2: Self-review against the approved spec.** Map each spec section to Tasks 1–7; confirm all reads are user-scoped, unsupported accounting is visibly unavailable, no inference response path waits on persistence, retention excludes admin audit, gateway default and llama-swap ownership are unchanged, and M7/M8 features are absent.
- [ ] **Step 3: Run full verification.** Run `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify sh -c 'go test ./... -count=1 && go build ./cmd/router && go mod verify'`; separately run `docker run --rm -v "$PWD:/src" -w /src golang:1.23 sh -c 'go test -race ./... -count=1'`; run `cd frontend && npm run build`; build the Docker image and smoke `/healthz`, `/readyz`, embedded `/`, unauthenticated `/api/v1/me/*`, unauthenticated `/api/v1/admin/*`, and `/v1/models` remains 404 with the gateway disabled.
- [ ] **Step 4: Obtain specialist reviews in order.** Run spec review first, then security review (owner scoping, untrusted response parser, cancellation, retention), interface/accessibility review, and code-quality review. Fix material findings and rerun affected checks before one completion audit. Do not attempt Git/worktree/commit operations because this workspace has no Git metadata.
