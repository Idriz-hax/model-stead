# Milestone 6 — Personal Dashboard Design

**Status:** Approved  
**Date:** 2026-11-03  
**Scope:** M6 only. M7 deployment integration and M8 advanced observability remain separate milestones.

## Goal

Give signed-in users a private view of their own recent inference activity, provider-reported token usage, and live requests they can cancel. Preserve the router's transparent proxy behavior and M5 privacy constraints.

## Approved direction

- **Layout:** Balanced personal dashboard: recent usage trend and recent activity together, with a separate request-history view. Continue the M5 Swiss Rationalist direction and existing embedded React application.
- **Storage:** Store one SQLite request record per authenticated, user-owned inference request. Enforce a 30-day retention period with automatic cleanup. Anonymous, IP-fallback, and other requests without an account owner are not part of personal history.
- **Token accounting:** Record only non-negative token counts explicitly reported by a supported upstream response. Do not estimate counts or calculate monetary cost. Fully reported, partial, and unavailable usage are distinguished.
- **Privacy:** Persist bounded operational metadata and provider-reported counts only. Never retain prompts, request/response bodies, credentials, session values, API-key material, or arbitrary headers.
- **Cancellation:** Users may cancel only their own currently queued or active requests. Queued work is canceled immediately; active cancellation requires confirmation and interrupts the upstream inference and client response.

## User interface

### Access and navigation

- Keep the existing login and forced-password-change flow. Password change revokes the current session, so show confirmation and return to login; after signing in again, an active non-admin account receives the personal dashboard instead of the M5 access-denied/M6-not-available page.
- Non-admin users see only **My dashboard** and **Request history**, plus their account/sign-out control; they do not see M5 admin navigation. Admins keep the M5 admin console and gain a **My dashboard** entry that shows only the admin's own usage. There is no user-impersonation or cross-user history view.
- The personal dashboard is available only to an active browser session. API keys remain inference credentials, not dashboard credentials. Anonymous or unowned identities have no personal history page.

### Overview

- Show request count and input/output token totals for the most recent 30 days.
- Show a daily token-usage trend for the same period. Aggregate each reported direction independently; show the number of requests with partial or unavailable usage so an incomplete total is not mistaken for complete accounting.
- Show recent request activity and a link to the full history.
- Do not show monetary cost or imply provider billing accuracy.

### Request history and live work

- Provide server-paginated history, default 50 rows and maximum 100, with filters for date range (within the 30-day retention window), endpoint, and outcome/status, plus a search string bounded to 128 characters for the requested-model label.
- Show request ID, requested model, endpoint, start/completion time, outcome/status, duration/queue wait where available, input/output/total tokens when reported, and clear partial/unavailable states otherwise.
- Store the requested model only if its trimmed value is 1–128 UTF-8 bytes and contains no control characters; otherwise store it as unavailable. It is client-supplied metadata and does not prove which upstream model served the request.
- Display current queued and active work by overlaying the in-memory registry onto its corresponding durable history row, without storing prompt/request content in the registry. Refresh user request state every five seconds while live work exists; no user-scoped SSE is required for M6.
- A cancel control is available only for a live request owned by the signed-in user. Confirm cancellation of active work with text that says the upstream inference and client response will be interrupted.

## API and authorization

- Add `GET /api/v1/me/usage`, `GET /api/v1/me/requests`, and `POST /api/v1/me/requests/{id}/cancel`.
- Usage and request-list reads accept a date range limited to the previous 30 days. History uses `limit`/`offset` (default 50, max 100), endpoint and outcome/status filters, and `search` bounded to 128 characters. The usage overview returns daily input/output totals, request count, and count of requests with partial or unavailable usage.
- History outcomes are `queued`, `active`, `completed`, `rejected`, `timed_out`, `cancelled`, `upstream_error`, or `interrupted`; HTTP status is nullable when no upstream response status was observed. Filters accept only allowlisted inference endpoints and the listed outcomes.
- Require an active browser session; reject anonymous, API-key-only, disabled, and forced-password-change sessions. Mutations require the existing exact-Origin and CSRF checks.
- Derive the owner from the validated session on the server. Never accept a user ID from a query parameter or request body as the authorization scope.
- Before cancellation, verify the live registry entry belongs to the session user. Unknown, completed, and other-user request IDs must not reveal ownership or cancel another user's work.
- Return bounded JSON errors without SQL details or secret-bearing fields. Use `Cache-Control: no-store` for private dashboard responses.

## Request capture and persistence

- Add a forward-only Goose migration for user-owned inference request history and any indexes required for recent-history and aggregation queries.
- Start a request-history row only after inference authentication and request-body validation succeed, and only when the resolved identity has a user owner. Record lifecycle changes for queue rejection/timeout/cancellation, dispatch, completion, and upstream error where observable. Mark unfinished records interrupted on restart rather than implying success. If a history write fails, log it and continue inference without claiming that request was recorded.
- Capture only known, bounded metadata: generated request ID (unique), owning user ID, allowlisted endpoint, requested-model label under the bound above, UTC lifecycle timestamps, outcome and nullable client-facing HTTP status, nullable input/output/total token counts, and usage state `reported`, `partial`, or `unavailable`.
- Observe only known usage fields for `/v1/chat/completions`, `/v1/completions`, `/v1/responses`, `/v1/messages`, and `/v1/embeddings`. Read each byte once as the existing proxy consumes it; do not read ahead, batch writes, change flush policy, buffer/re-serialize the full body, or wait for persistence. Bounded inline parsing overhead is permitted. Keep at most one 64-KiB SSE event and a 4-KiB usage object; an oversized event/object, unsupported content encoding, malformed/unknown format, or absent usage means unavailable and must not break inference.
- For those endpoints, recognize only their documented root/event usage fields (OpenAI chat/completions `usage.prompt_tokens`/`completion_tokens`; Responses `usage.input_tokens`/`output_tokens`; Anthropic Messages `usage.input_tokens`/`output_tokens`; embeddings `usage.prompt_tokens`/`total_tokens`). Do not count arbitrary nested `usage`, cached/tool subcounts, or infer usage by modifying client requests (for example, do not force streaming usage options).
- Do not treat `/v1/models` or `/v1/messages/count_tokens` utility responses as completed inference usage. Validate reported counts as non-negative signed 64-bit integers. If total is not directly reported, derive it only when both input and output counts are present; otherwise leave it unavailable. On malformed or conflicting recognized counts, mark usage unavailable rather than publishing partial provisional totals.
- Overlay current queued/active registry state onto the corresponding history row by request ID; do not render a second duplicate row for one live request.
- Update the history row after the response lifecycle completes. A telemetry persistence failure must not rewrite or corrupt the client-facing upstream response; log it without exposing the database error to the client.
- Purge request rows older than 30 days at startup and periodically. Retention applies to history records, not administrator audit events.

## Data and consistency rules

- Personal aggregates and rows are computed server-side with `user_id` taken from the authenticated session.
- Totals include only provider-reported values. Unknown counts remain null and are excluded from sums; the overview reports the number of requests with partial or unavailable usage.
- If only one token direction is reported, preserve that direction and leave the other unavailable; do not silently treat unknown as zero.
- Use a 30-day default range and reject query ranges outside the retained window; store timestamps as UTC Unix milliseconds and return RFC3339 UTC. History `from` is inclusive and `to` exclusive. A row's lifecycle outcome and observed response status are distinct (for example, an upstream error can still have an HTTP status).
- The history row is not an audit log. It may be deleted by the 30-day retention job; existing append-only administrator audit behavior is unchanged.

## Loading, empty, error, and accessibility behavior

- Use loading states that do not imply zero activity before results load.
- Empty states explain that no owned requests exist in the selected period.
- Show unavailable token usage separately from zero tokens; identify that values are provider-reported and may not be present for every request.
- Load failures provide retry and preserve filter selections. Cancellation failures leave the row visible and explain the failed operation.
- Disable duplicate submissions, announce results with accessible status text, use semantic headings/labels/tables, keyboard-operable controls, visible focus, and confirmation for active-request cancellation.
- On small screens, keep navigation, filters, summaries, and request tables usable without page-level horizontal overflow.

## Verification plan

- Store/migration tests: clean replay, owner-scoped insert/update/list, bounded pagination/filter validation, provider-usage nullability, 30-day cleanup, and interrupted requests after restart.
- Proxy tests: supported usage formats for JSON and streaming responses, missing/malformed usage, fragmented SSE chunks, bounded parser behavior, unchanged response bytes/headers/status/flush behavior, and upstream/client cancellation.
- Authorization tests: anonymous/API-key-only/disabled/forced-change denial, exact session ownership, cross-user listing isolation, and cross-user cancellation rejection.
- Lifecycle tests: queue/full/timeout/cancel/error/success persistence, completion metadata, owner-only cancellation, concurrent finish/cancel, and retention cleanup.
- Frontend build and browser smoke at desktop/mobile widths: user/admin gates, overview/history, filters/pagination, usage-unavailable states, live cancellation confirmation, loading/empty/error/retry states, and keyboard accessibility.
- Full project gates: `go test ./...`, `go test -race ./...`, `go build ./cmd/router`, `go mod verify`, `cd frontend && npm run build`, Docker image build and runtime smoke. Security and code-quality reviews precede the single completion audit.

## Explicit non-goals

- M7 deployment integration and M8 Prometheus/advanced observability.
- Monetary cost estimates, tokenizer-based estimates, provider-rate catalogs, budgets, quotas, or capacity changes.
- Prompt or response body capture, response translation, or public access to another user's data.
- Anonymous/IP/device-only personal dashboards, exports, saved reports, admin impersonation, or new user-scoped SSE.
- Changes to llama-swap ownership or enabling the inference gateway by default.
