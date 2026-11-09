# Milestone 5 — Admin Dashboard Design

**Status:** Approved
**Date:** 2026-11-03  
**Scope:** M5 only. M6 personal dashboards, M7 TrueNAS deployment integration, and M8 advanced observability remain separate milestones.

## Goals

Give administrators a web interface to inspect and control users, trusted device mappings, API keys, scheduler configuration, live scheduling, and current requests without editing SQLite directly. Changes must be validated and auditable. Live dashboard state must update in real time.

## Approved decisions

- **Architecture:** Single-process control plane in the existing Go service. JSON REST endpoints handle reads and mutations; server-sent events (SSE) push live state. SQLite holds durable administrator-managed state, settings, and audit events. Active/queued request state and cancellation functions remain in memory. Request history and usage aggregates remain M6/M8 work.
- **Scheduler settings:** Persist in SQLite. YAML initializes them only when no persisted settings exist. Apply validated changes live. Lowering capacity does not preempt active leases; dispatch pauses until current usage drops below the new limit.
- **User onboarding:** Admin creates accounts with a temporary password. User must change it at first login. Only the password hash is stored.
- **User disable:** Disable without hard deletion; immediately revoke the user's API keys and browser sessions and cancel their queued/active work. Preserve the user and audit history. Prevent disabling/demoting the last active administrator.
- **Keys:** Show plaintext key only once at creation. Later responses expose metadata/prefix only. Revocation immediately prevents new authentication.
- **Request cancellation:** Admin can cancel queued or active requests. Queued cancellation is immediate; active cancellation requires confirmation and cancels upstream work without retry. Non-admin self-service UI/API is deferred to M6; a client can cancel its current inference request by closing its connection.
- **Design direction:** Swiss Rationalist: bright neutral surfaces, clear grid, restrained blue accent, disciplined typography, compact but readable data tables, minimal decoration. The user selected the “Capacity desk” direction in the visual comparison.

## User interface

Use the existing embedded React application with persistent left navigation: Overview, Requests, Users, Devices, API keys, Scheduler, Audit. Default landing view is Overview. Add a login screen; users with `must_change_password` are routed only to a forced password-change screen until the change succeeds. After a non-admin user changes the temporary password, show an access-denied/M6-not-yet-available screen with logout rather than exposing admin navigation.

### Overview

- Capacity, active/reserved, queued, and oldest-wait indicators.
- Per-identity queue/active summary.
- Current active and queued requests with a route to the Requests view.
- Clear live/stale connection status and last-update time.

### Requests

- Live list of queued and active requests only; no durable history in M5.
- Show request ID, identity/user, endpoint, state, enqueue/dispatch time, and queue wait. Do not retain arbitrary request model strings in scheduler memory or monitoring.
- Queued requests have immediate cancel controls. Active cancellation requires confirmation, naming that the upstream inference and client response will be interrupted.
- Never display or persist prompt/request content in scheduler monitoring or audit logs.

### Users

- Searchable/paginated user table with username, display name, role, status, scheduling weight, queue/active overrides, and current queue/active counts.
- Create and edit accounts; set temporary password and require password change at first login.
- Disable/reactivate accounts; disabling revokes credentials/sessions and cancels in-flight work. No hard-delete operation.
- Reject invalid fields, duplicate usernames, unsafe role changes, and changes that would leave no active administrator.

### Devices

- Manage the existing trusted-device records: label, exact IP address, optional associated user, trusted/untrusted status, timestamps.
- Validate IP addresses and uniqueness. Changing trust to untrusted removes that mapping from future device identity resolution.

### API keys

- List key metadata only: owner, name, prefix, status, created/last-used/expiry timestamps.
- Create a named key for an active user with optional expiry; show plaintext once with an explicit copy/save warning.
- Revoke keys with confirmation; never reveal or log a secret again.

### Scheduler

- Edit global capacity, default weight, per-identity queued limit, contended active limit, global queue limit, max wait, work-conserving behavior, and cancellation grace.
- Show persisted/effective values, validation errors, save state, and whether requests are waiting for capacity to decrease after a lower limit.
- Settings survive restart; persisted values override YAML defaults after first initialization.

### Audit

- Append-only, paginated, filterable events with timestamp, actor, action, target, and redacted before/after values.
- Include user/device/key/scheduler/request-cancel changes. Never include passwords, password hashes, session/CSRF tokens, API key secrets/hashes, prompts, or full request bodies.
- No update/delete API for audit events.

## API and authorization

All management endpoints are under `/api/v1/admin/` and require an active dashboard session with role `admin`. Mutations require the configured exact Origin and existing CSRF token/header checks. API keys authenticate inference clients, not admin dashboard sessions. SSE is admin-session-only and sends an initial scheduler/request snapshot plus bounded change events; state changes are pushed promptly, heartbeat comments are sent every 15 seconds, and reconnect starts with a fresh snapshot. On stream failure, the UI marks data stale and falls back to a 5-second refresh until the stream recovers.

Routes: `GET /overview`; `GET /events` (SSE); `GET /requests`; `POST /requests/{id}/cancel`; `GET,POST /users`; `GET,PATCH /users/{id}`; `POST /users/{id}/disable`, `/enable`, and `/reset-password`; `GET,POST /devices`; `GET,PATCH /devices/{id}`; `POST /devices/{id}/trust` and `/untrust`; `GET,POST /api-keys`; `POST /api-keys/{id}/revoke`; `GET,PUT /scheduler`; and `GET /audit`. All are beneath `/api/v1/admin/`. Lists use a default page size of 50 and maximum of 100; search filters are bounded to 128 characters. Use explicit action endpoints for destructive transitions rather than hard deletes. API errors are JSON and do not reveal secrets or SQL details.

## Persistence and consistency

Add a forward-only Goose migration for persisted scheduler settings, audit events, and a user `must_change_password` indicator. Existing migrations are not edited. User/key/device/config mutation and its audit event are committed together. After a successful commit, update in-memory scheduler policy/config or cancel affected live requests. Runtime scheduler policy changes must be validated before persistence so the DB and live scheduler cannot diverge on invalid values.

At startup, seed the settings row from validated YAML only when absent; otherwise load persisted values. Request registry holds only currently queued/active metadata and cancellation handles, removes completed/canceled entries, and is not restored from SQLite after restart.

## Validation and safety limits

- Username: 3–64 ASCII letters/digits plus `.`, `_`, and `-`, case-insensitive unique; display name: 1–80 characters.
- Scheduling weight: finite, 0.1–100 inclusive.
- Per-user queue override: `0` means inherit default; otherwise 1–10,000. Per-user active override: `0` means inherit default; otherwise 1–64 and no greater than global capacity.
- Global capacity: 1–64; global queue: 1–10,000.
- Max wait: 1 second–24 hours; cancellation grace: 1 second–5 minutes.
- Validate scheduler policy against the supported weighted round-robin policy. No telemetry can increase configured capacity in M5.
- Validate IP address fields as individual IPs (device mappings are exact-IP records, not CIDRs).
- API key name and resource identifiers have explicit length bounds; expiry, when set, must be in the future.
- On user disable, key/session revocation and audit are transactional; current queued/active work is canceled after commit. If in-memory cancellation fails, return/report operational failure and retain an auditable state for reconciliation.

## Loading, empty, error, and accessibility behavior

- Initial loading uses restrained skeleton rows; avoid implying zero activity while data is pending.
- Empty tables explain the resource and offer the relevant create action where authorized.
- API failures preserve entered form values and show field or operation errors; retryable load errors provide retry.
- SSE disconnection displays a visible stale indicator, last update time, and periodic-refresh fallback; reconnect restores a snapshot.
- Disable submit while saving, prevent duplicate mutations, and announce outcomes with accessible status text.
- Destructive user disable/key revoke/request cancellation requires clear effect text and confirmation as described above.
- Use semantic headings, labels, table headers, keyboard-operable controls/dialogs, visible focus, and status indicators not conveyed by color alone.

## Verification plan

- Go tests: admin role/session/CSRF enforcement; CRUD validation; duplicate and last-admin protections; temporary-password enforcement; one-time API key response; immediate revocation; transactional audit and redaction; scheduler persistence/restart and live updates; no-preemption capacity reduction; SSE snapshot/reconnect; queued/active cancellation and cleanup; disabled-user request cancellation.
- Race tests for scheduler snapshots, request registry, SSE clients, user disable/cancel, and concurrent config changes.
- Frontend tests/build for responsive tables/forms, keyboard use, loading/empty/error/stale states, confirmation, and one-time key reveal.
- Full project gates: `go test ./...`, `go test -race ./...`, `go build ./cmd/router`, `go mod verify`, `cd frontend && npm run build`, Docker image build and runtime smoke.
- Reviews: spec, security, interface/accessibility, code quality, and completion audit.

## Explicit non-goals

- M6 personal usage overview, request history, token accounting, historical charts, or self-service request dashboard/API.
- M7 TrueNAS Compose/networking/backup/restore work.
- M8 Prometheus expansion, latency percentiles, usage aggregation, upstream polling, or slot telemetry adapters.
- Any llama-swap deployment/inference configuration changes.
- Public unauthenticated management endpoints or arbitrary proxy routes.
