# M5 Admin Dashboard Implementation Plan

> **Archive status:** M5 is implemented. This dated plan records its execution workflow; unchecked boxes are historical checklist state, not current open tasks. See [milestone index](../milestones.md).

**Goal:** Deliver an authenticated administrator console for live scheduling, user/device/API-key management, persisted scheduler configuration, request cancellation, and auditable changes.

**Architecture:** Keep one Go process and the existing embedded React app. Add an `internal/admin` service for validated, transactional SQLite operations and redacted audit writes; add admin-only JSON/SSE handlers plus a bounded in-memory live-request registry in `internal/server`. Persist settings/user onboarding state/audit in a forward-only Goose migration; keep current requests and cancellation handles in memory. Build the UI with existing React dependencies and the approved Swiss Rationalist direction.

**Tech Stack:** Go 1.23, `net/http`, SQLite/Goose, current scheduler/auth packages, React 19 + TypeScript, SSE, Docker.

---

## Scope guard

Implement M5 only. User dashboard, usage/history/tokens/charts (M6), TrueNAS deployment changes (M7), and advanced telemetry/Prometheus expansion (M8) stay out of scope. Non-admin users can abort their own inference by closing the existing request connection; M5 does not add a self-service request page or cancel API. Preserve the gateway-disabled default and llama-swap ownership. Do not hard-delete users, keys, or device trust history. This workspace has no `.git`, so plan execution must not include commit/worktree commands.

## File map

- `internal/database/migrations/00003_admin.sql` — forward-only scheduler settings, audit events, and `users.must_change_password`.
- `internal/admin/store.go`, `validation.go` — admin DTOs, field validation, transactional CRUD/config/audit operations; `store_test.go` — SQLite-backed validation, durability, and audit tests.
- `internal/auth/identity.go` / `session.go` — only if a narrowly needed public auth helper is required; do not change API-key/session security behavior otherwise.
- `internal/server/admin.go` — admin-session/RBAC/CSRF HTTP handlers for REST and SSE; `requests.go` — bounded live request registry and cancellation; matching tests exercise HTTP and lifecycle behavior.
- `internal/scheduler/scheduler.go` — validated live settings update and change notification/snapshot support; scheduler tests cover no-preemption changes and observers.
- `cmd/router/main.go`, `internal/config/config.go` — seed/read persisted settings and wire the admin store, scheduler, and registry at startup.
- `frontend/src/main.tsx`, new focused `api.ts`, `AdminApp.tsx`, page components, and `style.css` — login/forced password change and admin navigation/pages without adding dependencies.
- `README.md`, `config.example.yaml` — explain bootstrap, settings precedence, admin operations, audit/privacy, and in-memory request monitoring.

## Execution order

1. Migration + transactional store and validation.
2. Forced-password user lifecycle and admin CRUD APIs (users/devices/keys).
3. Persisted scheduler settings and safe live reconfiguration.
4. Live request registry, cancellation, and user-disable cleanup.
5. Admin REST/SSE routes, RBAC, and aggregate snapshots.
6. React login/forced-change flow and dashboard pages/states.
7. Documentation, full validation, and specialist reviews.

## Tasks

### Task 1: Admin schema, validators, and audit store

**Files:** Create `internal/database/migrations/00003_admin.sql`; create `internal/admin/store.go`, `internal/admin/validation.go`, and `internal/admin/store_test.go`.

- [ ] **Step 1: Write failing SQLite tests** for migration columns/tables; scheduler settings seed/read; invalid weight/capacity/name/IP; atomic resource mutation plus audit; audit redaction; pagination bounds.
- [ ] **Step 2: Run the red tests:** `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./internal/admin -run 'Test(Migrate|Settings|Validate|Audit)' -count=1`. Expected: package/test symbols do not exist yet.
- [ ] **Step 3: Add migration.** Keep migrations 00001/00002 unchanged. Add `users.must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0,1))` and a unique index on `lower(username)`; add a singleton scheduler settings row with policy, capacity, weights, queue limits, max wait, work-conserving flag, cancellation grace, and update metadata; add append-only audit rows with actor FK, action/target fields, redacted before/after JSON, and created timestamp. Index audit time, actor, and target.
- [ ] **Step 4: Implement store and validation.** Use typed request/value structs; validate before opening a transaction. Write each state change and its audit row in one transaction. Provide cursor/page-size-bounded audit listing (default 50, max 100). Store only allowlisted redacted fields; never accept arbitrary audit JSON from callers.
- [ ] **Step 5: Run focused tests:** `go test ./internal/admin ./internal/database -count=1`. Expected: all migration, validation, rollback, and audit tests pass.

### Task 2: User lifecycle and temporary-password enforcement

**Files:** `internal/admin/store.go`, `internal/admin/store_test.go`, `internal/server/auth.go`, `internal/server/auth_test.go`, `internal/database/migrations/00003_admin.sql`.

- [ ] **Step 1: Write failing tests** for unique normalized username creation, temporary password hashing, must-change flag, role/status/scheduling edits, duplicate rejection, last-active-admin protection, disabled-user session/key revocation, and password-change clearing the flag.
- [ ] **Step 2: Verify red:** `go test ./internal/admin ./internal/server -run 'Test(AdminUser|LastAdmin|TemporaryPassword)' -count=1`. Expected: missing store methods/forced-change behavior.
- [ ] **Step 3: Implement user operations.** Accept usernames matching `[A-Za-z0-9._-]{3,64}` and compare uniqueness case-insensitively; display names 1–80; finite weight 0.1–100; queue override 0 or 1–10,000; active override 0 or 1–64 and no greater than configured capacity. Create/reset temporary passwords through `auth.HashPassword`; persist hashes only and set `must_change_password=1`. Disable in one transaction: set disabled, revoke API keys, revoke sessions, write redacted audit event. Reject disabling/demoting the last active admin.
- [ ] **Step 4: Enforce forced password change.** Include `must_change_password` in login/`me` responses. Until cleared, allow only `me`, password change, and logout; deny admin APIs. On successful password change, clear the flag in the same transaction as password update/session revocation. Preserve current M3 CSRF/origin and status/error behavior.
- [ ] **Step 5: Run focused tests:** `go test ./internal/admin ./internal/server ./internal/auth -count=1`.

### Task 3: Device and API-key management

**Files:** `internal/admin/store.go`, `internal/admin/store_test.go`, `internal/server/admin.go` (added in Task 5), `internal/server/admin_test.go` (added in Task 5).

- [ ] **Step 1: Write failing store tests** for device create/update/trust/untrust and unique parsed IPs; API-key metadata list, one-time secret creation, optional future expiry, revocation, and no secret/hash in list/audit results.
- [ ] **Step 2: Verify red:** `go test ./internal/admin -run 'Test(AdminDevice|AdminAPIKey)' -count=1`.
- [ ] **Step 3: Implement operations.** Device IPs must parse as individual IPv4/IPv6 addresses; retain untrusted mappings rather than hard-delete. API-key names are 1–80 characters; owners must be active; expiry must be in the future. Generate with existing 256-bit key generator and store only SHA-256 hash; return plaintext only from the create call. Revoke by setting status/revoked timestamp. Audit metadata/prefix only.
- [ ] **Step 4: Verify:** `go test ./internal/admin ./internal/auth -count=1`.

### Task 4: Persisted scheduler settings and live apply

**Files:** `internal/admin/store.go`, `internal/admin/store_test.go`, `internal/scheduler/scheduler.go`, `internal/scheduler/scheduler_test.go`, `internal/config/config.go`, `internal/config/config_test.go`, `cmd/router/main.go`.

- [ ] **Step 1: Add failing settings tests** for YAML-to-DB seed on first boot, persisted values taking precedence after restart, invalid updates rejected without changing runtime/DB, and capacity reduction never preempting current leases.
- [ ] **Step 2: Verify red:** `go test ./internal/admin ./internal/scheduler ./internal/config -run 'Test(PersistedScheduler|UpdateConfig|CapacityReduction)' -count=1`.
- [ ] **Step 3: Add scheduler live update API.** Validate a complete candidate config before locking; atomically replace the config under the scheduler mutex, keep active/reserved lease counts unchanged, dispatch only while `active+reservations < newCapacity`, and broadcast change notifications. Expose a race-safe watch/snapshot API; do not expose internal channels or prompt/request bodies.
- [ ] **Step 4: Wire persistence.** After database migration, insert YAML settings only if the singleton row is absent; otherwise load persisted settings. For updates, validate, persist with an audit event in one transaction, then apply the already-validated settings in memory. Preserve all M4 defaults and `float64` weight precision.
- [ ] **Step 5: Verify:** `go test ./internal/admin ./internal/scheduler ./internal/config ./cmd/router -count=1`.

### Task 5: Live request registry and cancellation

**Files:** Create `internal/server/requests.go`, `internal/server/requests_test.go`; modify `internal/server/server.go`; add request lifecycle tests in `internal/server/server_test.go`.

- [ ] **Step 1: Write failing registry tests** for queued/active state transitions, cancellation by request ID, cancellation of all work for a user, concurrent finish/cancel idempotence, and removal after completion.
- [ ] **Step 2: Verify red:** `go test ./internal/server -run 'TestRequestRegistry|TestCancelUserRequests' -count=1`.
- [ ] **Step 3: Implement bounded memory registry.** Track only ID, user/identity ID, endpoint, state, enqueue/start timestamps, scheduler lease, and `context.CancelFunc`; never store bodies, prompts, API keys, or unbounded model strings. Register before scheduler submission; transition to active immediately before proxy; remove on completion. Protect map/transitions with a mutex. Limit registry entries consistently with scheduler queue/capacity bounds.
- [ ] **Step 4: Add cancellation integration.** Queued cancel cancels the derived request context and lets scheduler remove the queue item. Active cancel calls the context cancel function; the proxy sees cancellation and existing lease release/cancellation grace remains authoritative. User disable cancels all requests after the transaction commits. Concurrent finish/cancel must release each lease exactly once.
- [ ] **Step 5: Verify:** `go test ./internal/server ./internal/scheduler -run 'Test(RequestRegistry|Cancel|Scheduler)' -count=1` and `go test -race ./internal/server ./internal/scheduler -count=1`.

### Task 6: Admin REST/SSE APIs and authorization

**Files:** Create `internal/server/admin.go`, `internal/server/admin_test.go`; modify `internal/server/server.go`, `cmd/router/main.go`, and `internal/admin/store.go` only as required by tested interfaces.

- [ ] **Step 1: Write failing HTTP tests** for non-admin/anonymous rejection; cookie-session admin access; Origin/CSRF rejection; each resource’s CRUD/action scope; redacted audit; cancellation of queued vs active; SSE initial snapshot/reconnect/heartbeat; and JSON errors without SQL detail.
- [ ] **Step 2: Verify red:** `go test ./internal/server -run 'TestAdmin(API|SSE|Authorization)' -count=1`.
- [ ] **Step 3: Implement admin-only route group** under `/api/v1/admin/`: overview, events, requests/cancel, users CRUD/disable/enable/reset-password, devices CRUD/trust/untrust, API keys list/create/revoke, scheduler get/update, audit list. Keep API-key auth insufficient for dashboard administration; require active admin browser session. Extract the current `registerAuth` closure’s session lookup, exact-Origin check, constant-time CSRF comparison, and JSON response helpers into private helpers shared by `auth.go` and `admin.go`; do not weaken their behavior. Use JSON with bounded body sizes and pagination (default 50/max 100).
- [ ] **Step 4: Implement SSE.** Send one initial snapshot, then bounded updates when scheduler/request state changes; send 15-second heartbeat comments. Close stream on request cancellation. On reconnect the client gets a new snapshot. Do not include request body, secrets, CSRF token, or user password data.
- [ ] **Step 5: Verify:** `go test ./internal/server ./internal/admin -run 'TestAdmin|TestSSE' -count=1`.

### Task 7: React admin dashboard and forced-change UI

**Files:** Modify `frontend/src/main.tsx`, `frontend/src/style.css`; create `frontend/src/api.ts`, `frontend/src/AdminApp.tsx`, and focused page/components under `frontend/src/admin/`.

- [ ] **Step 1: Add pure UI tests only if an existing test runner is available; do not add a dependency solely for trivial components.** Otherwise define and smoke-test forms in the browser after each vertical slice.
- [ ] **Step 2: Implement session gate.** Login calls existing auth endpoint with credentials and Origin; show only login for anonymous users. Route `must_change_password` users to the forced password form; non-admin after password change sees access denied/logout, not admin pages.
- [ ] **Step 3: Build approved shell.** Swiss Rationalist white/neutral grid, restrained blue accent, persistent left nav, responsive content, visible focus, semantic page titles/tables/forms. Add Overview and Requests with SSE live/stale indicators and 5-second fallback polling.
- [ ] **Step 4: Add resource pages.** Users (create/edit/disable/reset temporary password); Devices (label/IP/user/trust); API keys (create and one-time secret reveal, metadata-only list, revoke); Scheduler (validated values/effective save state); Audit (filters/pagination). Keep destructive confirmation and field errors in their owning components.
- [ ] **Step 5: Verify UI build:** `cd frontend && npm run build`; smoke each page at desktop/mobile widths with admin session and test loading/empty/error/stale states in a browser.

### Task 8: Documentation, full verification, and review

**Files:** Modify `README.md`, `config.example.yaml`; no unrelated deployment/llama-swap changes.

- [ ] **Step 1: Document** admin setup, forced password change, scheduler settings persistence precedence, user/device/key operations, one-time key display, audit redaction, request retention/cancellation, SSE fallback, and `/metrics` admin-key authorization.
- [ ] **Step 2: Run full required checks:** `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go test ./... -count=1`; `docker run --rm -v "$PWD:/src" -w /src golang:1.23 go test -race ./... -count=1`; `docker run --rm -v "$PWD:/src" -w /src afr-builder:verify go build ./cmd/router`; same builder `go mod verify`; `cd frontend && npm run build`; then `docker build -t ai-fairness-router:m5 .`, run it with a temporary data volume and port mapping, and verify `/healthz` and `/readyz` return 200 JSON, `/` contains the app title, unauthenticated `/api/v1/admin/overview` returns 401/403, and `/v1/models` returns 404 while gateway is disabled. Stop/remove the smoke container and volume.
- [ ] **Step 3: Review actual code against the design** with spec review first, then security and interface/accessibility review, then code quality, then one final completion audit. Fix material findings and rerun affected tests; keep M6–M8 out of scope.

## Plan self-review

- Spec coverage: all M5 pages, management resources, scheduler persistence, live request list/cancel, SSE, audit, validation, forced password onboarding, privacy, failure states, security, accessibility, and verification have explicit tasks.
- Out of scope: user usage/history/token UI, TrueNAS, advanced metrics/telemetry, and self-service cancel API are explicitly deferred.
- Type/data consistency: scheduler settings preserve `float64` weights; live requests contain bounded metadata only; user request cancellations use context cancellation; DB mutations and audit writes share a transaction.
- Repository constraint: no Git metadata is present; do not attempt commits or worktree creation. The project remains milestone-by-milestone; implement M5 only.
