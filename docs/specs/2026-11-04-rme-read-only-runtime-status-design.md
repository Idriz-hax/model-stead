# Runtime Management Extension — Read-Only Status Design

**Status:** Approved — implementation authorized (2026-11-04)  
**Date:** 2026-11-04  
**Scope:** First RME stage only: administrator-visible, read-only llama-swap service and model status. Runtime controls remain a separate future stage requiring its own approved specification.

## Goal

Let an administrator inspect whether the configured llama-swap service is reachable, which models it reports as configured, and which models it reports as running, without making the router the runtime owner or changing inference behavior.

## Current foundation and API evidence

- model-stead already has an optional inference gateway with a configured upstream URL and server-side API key. The gateway remains disabled by default and is not the enable switch for this status integration.
- The TrueNAS deployment connects to an existing, separately managed llama-swap service; it does not deploy or configure llama-swap.
- The supported upstream release is llama-swap v269, pinned to commit `9fbc47ce96d10160e95994c7a26513c972201a74`. Tagged source verifies `GET /health` returns plain-text `OK` without API-key middleware; `GET /v1/models` returns configured models in `data[].id`; `GET /running` returns `running[].model` objects. `/health` is service-level reachability, not proof that every model is ready.
- The v269 tag's API-key middleware protects `/v1/models` and `/running` when `apiKeys` is configured; Bearer auth is supported. Contract fixtures and tests use these tagged schemas. No live probe against an operator's llama-swap instance has been performed.

## Approved design

### Integration boundary and configuration

- Add a separate `runtime_management` integration configuration with `enabled: false` by default, a llama-swap base URL, and `api_key_env` defaulting to `RUNTIME_MANAGEMENT_API_KEY`. It is independent of `gateway.enabled` and does not reuse inference enablement as a control.
- When enabled, require a non-empty API key environment variable and send it server-side as a Bearer token. Never return the secret or raw upstream response to the browser. Operator documentation must state that llama-swap API-key authentication must be enabled and that network/firewall isolation remains necessary.
- Validate the configured URL as an absolute HTTPS URL with a host, no userinfo, query, or fragment. Permit HTTP only for `localhost` or loopback IPs so the Bearer key is not sent in cleartext over a network. The application appends only the fixed read-only paths in this spec. Do not accept a URL or path from a browser request.
- Pin llama-swap v269 / commit `9fbc47ce96d10160e95994c7a26513c972201a74`; derive contract parsing and tests from that tag, not unpinned `main`.

### Server-side status adapter

- Add an adapter in the Go server reachable only through the active-admin-session-protected `GET /api/v1/admin/runtime` endpoint. Enforce existing role and forced-password-change restrictions.
- The adapter may call only `GET /health`, `GET /v1/models`, and `GET /running` on the configured base URL. No redirects, arbitrary paths, management API routes, or user-provided upstream values. Call the three endpoints concurrently through a dedicated HTTP client with a 3-second per-request timeout and a 1 MiB maximum response body per request.
- Disable redirect following so credentials cannot be sent to a different host. Do not retry requests. Do not add background polling, persistence, metrics, SSE, or caching in this stage; the admin page requests a fresh snapshot on entry and on explicit refresh.
- Normalize upstream responses into a small bounded DTO. Return service health, configured model identifiers, currently running model identifiers, and a sample timestamp. Bound the number and length of model identifiers, reject malformed or oversized responses, and never expose arbitrary upstream fields, headers, error bodies, or secrets.
- `/health` represents only llama-swap service reachability. Report model running state only from `/running`; do not infer per-model readiness from `/health` or from the configured model list. The implementation must use schemas verified against the pinned release.
- Distinguish disabled, reachable, unauthorized, unavailable/timeout, unsupported or malformed response, and valid empty configured/running lists. Errors must not be represented as an empty model list or healthy status.

### Administrator interface

- Add a dedicated **Runtime** page to the existing admin console. Do not add runtime data to the M8 Router Health panel or personal user dashboard.
- Show service reachability and timestamp, configured models, and currently running models. Make clear that this page is read-only and that “running” does not necessarily mean model-level readiness.
- Show distinct disabled, initial loading, valid empty-list, stale-last-snapshot, unauthorized, timeout/unavailable, and malformed/unsupported states. On refresh failure, retain the last good snapshot with an explicit stale/error cue; keep the rest of the admin console usable.
- Provide a manual refresh control; no automatic polling or chart is required. Use existing visual patterns, responsive single-column behavior on narrow screens, semantic labels/status announcements, and no page-level horizontal overflow.

## Security and operational requirements

- Runtime status access is administrator-session-only. Existing `/metrics` API-key behavior and inference gateway auth remain unchanged.
- The integration is operator-configured outbound access. It calls only fixed paths on the configured host, has request and response bounds, rejects redirects, and never accepts a destination from an API caller.
- The llama-swap API key stays in the server environment and is never returned, persisted, logged, or included in errors. Error messages are bounded and generic; avoid logging response bodies or credentials.
- A misconfigured/unreachable llama-swap service must not prevent model-stead startup, admin login, existing inference routing, or scheduler operation. If runtime integration is enabled but unavailable, report it in the Runtime page rather than failing unrelated paths.
- Keep gateway default disabled. No database migration, new dependency, GPU telemetry, process/container lifecycle, model loading/unloading, profile changes, pause/resume, logs, or llama-swap configuration changes.

## Acceptance criteria

1. `runtime_management.enabled` defaults off and remains independent of `gateway.enabled`; existing inference gateway behavior and llama-swap ownership are unchanged.
2. Only an active admin session can retrieve runtime status; unauthenticated, non-admin, and forced-password-change sessions cannot.
3. When enabled, the adapter calls only the three documented GET paths on the configured base URL, with a pinned/tested llama-swap release, bounded timeout/body, no redirects/retries, and server-only Bearer credential handling.
4. Valid configured/running models are shown as separate lists; empty lists are distinct from disabled, unauthorized, timeout, malformed, oversized, or incompatible responses.
5. `/health` is described only as service reachability; model readiness is never inferred from it.
6. The admin Runtime page has loading, disabled, empty, stale, unauthorized, unavailable, and successful states; the rest of the admin app remains usable on failure and the layout works at mobile widths.
7. Automated tests cover configuration/defaults, pinned upstream contract parsing, auth, redirects, timeout, oversized/malformed responses, fixed-path behavior, secret non-disclosure, and failure isolation. Browser smoke covers the Runtime page states and responsive layout.
8. Go tests/build, frontend build, and Docker build/smoke pass. A live test against the selected llama-swap release is recorded separately from mocked contract tests.

## Verification references

- llama-swap v269 release: <https://github.com/mostlygeek/llama-swap/releases/tag/v269>
- v269 API handlers: <https://github.com/mostlygeek/llama-swap/blob/v269/internal/server/api.go>
- v269 health and route registration: <https://github.com/mostlygeek/llama-swap/blob/v269/internal/server/server.go>
- v269 API-key middleware: <https://github.com/mostlygeek/llama-swap/blob/v269/internal/server/auth.go>
- v269 key extraction: <https://github.com/mostlygeek/llama-swap/blob/v269/internal/swaputil/http.go>

The implementation is scoped to the pinned v269 contracts above. Live verification against a configured llama-swap instance remains an operational check; it is not represented as completed by mocked contract tests.
