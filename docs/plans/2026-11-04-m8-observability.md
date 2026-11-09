# M8 Observability Implementation Plan

> **Archive status:** M8 implementation and review are complete. This plan records the execution workflow and verified checks. See [milestone index](../milestones.md).

**Goal:** Add bounded router inference outcomes/timing to Prometheus and admin Overview without changing fairness or inference behavior.

**Architecture:** A process-local collector records one terminal outcome per allowed inference POST and fixed-bucket duration/queue-wait histograms. `/metrics` and the session-protected Overview consume its snapshot directly; a five one-minute bucket ring reports recent outcomes, while timing means are labeled since restart. No persistence or third-party metrics library.

**Tech Stack:** Go standard library, existing `net/http`, React/TypeScript frontend, existing CSS and test/build tools.

---

## File map

| Path | Responsibility |
|---|---|
| `internal/observability/metrics.go` | Bounded outcome counters, fixed histogram buckets, recent minute ring, snapshots, Prometheus text rendering. |
| `internal/observability/metrics_test.go` | Collector correctness, rollover, bounds, exposition tests. |
| `internal/server/server.go` | Collector initialization, `/metrics` extension, inference lifecycle and queue timing instrumentation independent of history. |
| `internal/server/server_test.go` | Authorization/compatibility and lifecycle integration tests. |
| `internal/server/admin.go`, `internal/server/admin_test.go` | Add collector snapshot to existing Overview API; preserve existing fields and admin-session guard. |
| `frontend/src/AdminApp.tsx` | Render bounded Router Health panel from Overview payload. |
| `frontend/src/style.css` | Minimal responsive styles reusing existing panel/metric conventions. |
| `README.md` or `docs/observability.md` | Document scrape endpoint, auth, metrics, labels, units, reset and timing definitions. |

**Overview JSON contract:** preserve existing `scheduler` and `requests`; add `observability` with `process_started_at` (RFC3339 UTC), `gateway_enabled` (boolean), `recent_window_started_at` and `recent_window_ended_at` (RFC3339 UTC), `recent_outcomes` (object with every fixed outcome key and integer counts), `request_duration_seconds` (`count`, `sum`), and `queue_wait_seconds` (`count`, `sum`). The frontend derives totals/errors and means from these values; missing `observability` means unavailable, while an explicit zero is valid.

## Task 1: Collector contract and tests

**Files:** Create `internal/observability/metrics_test.go`, then `internal/observability/metrics.go`.

- [x] Test bounded outcome recording, histogram count/sum, recent-window expiry, excluded routes, metric names/types, and `+Inf` exposition. Lifecycle integration tests cover exactly-once success, rejection, timeout, cancellation, interruption, and upstream error outcomes.
- [x] Run `go test ./internal/observability` and confirm tests fail because package/API is absent.
- [x] Implement one mutex-protected collector with fixed endpoint/outcome enums, preallocated metric cells, fixed histogram boundaries, and five minute buckets. Snapshot returns process start, gateway enabled, recent counts, and lifetime histogram count/sum; no request identity or raw strings are retained.
- [x] Verify `go test ./internal/observability` passes.

## Task 2: Server lifecycle integration

**Files:** Modify `internal/server/server.go`; add focused integration cases to `internal/server/server_test.go`.

- [x] Test success, rejected key, queue timeout, cancellation, downstream interruption, upstream failure without history, and exclusions; direct lifecycle cases assert exactly one terminal outcome and queue timing is collected only after submission.
- [x] Run focused server tests and confirm the new assertions fail before instrumentation.
- [x] Initialize the collector once in `NewWithSchedulerConfig`; preserve existing `/metrics` API-key authorization and append collector exposition to the existing scheduler lines.
- [x] Instrument only fixed inference POST routes. Use a deferred finalizer that classifies the terminal result exactly once and measures handler elapsed time; track normal proxy return separately so panic/abort and incomplete downstream delivery are not reported as completed. Measure scheduler submit wait including terminal wait results. Keep metrics independent of optional history writes and preserve existing cancellation/history behavior.
- [x] Run focused server tests and confirm all new and existing cases pass.

## Task 3: Admin Overview data contract

**Files:** Modify `internal/server/admin.go`; add tests in `internal/server/admin_test.go`.

- [x] Test that Overview includes `observability` with gateway flag, process start, recent outcomes and since-restart duration/queue-wait count/sum, while preserving `scheduler` and `requests`.
- [x] Verify metrics are returned only through the existing active-admin-session guard; preserve exact-Origin/CSRF rules for mutations (there is no new mutation).
- [x] Add the collector snapshot to existing Overview response; do not add a new endpoint and do not read M6 history for metrics.
- [x] Run focused admin tests.

## Task 4: Admin Router Health panel

**Files:** Modify `frontend/src/AdminApp.tsx`, `frontend/src/style.css`.

- [x] Add an overview panel below capacity cards and above the live requests/identity sections. Show recent outcome totals over “current minute + previous four minutes” and since-restart mean request/queue wait, with units and restart reset note. Do not show percentile claims.
- [x] Show loading placeholders, “No data yet” when sample count is zero, preserve last values with the existing stale indicator, and show a bounded inline unavailable message without hiding the rest of Overview.
- [x] Use visible labels/status text; keep responsive single-column behavior on narrow screens; no charts or horizontal scrolling.
- [x] Build frontend and inspect a focused browser smoke at desktop/mobile widths.

## Task 5: Documentation and full verification

**Files:** Create/update `docs/observability.md` or README observability section.

- [x] Document metric names, fixed labels, bucket boundaries, units, endpoint auth, process-reset behavior, exact recent window, and limitations (router latency is not model-generation latency).
- [x] Run `go test ./...`.
- [x] Run `go build ./cmd/router`.
- [x] Run `cd frontend && npm run build`.
- [x] Review the diff against `docs/specs/2026-11-04-m8-observability-design.md`; confirm no migration, new dependency, M1–M7 behavior change, or llama-swap/GPU work.

## Out of scope

Repository, Go module, and directory rename completed after M8 reconciliation: `model-stead`, `github.com/Idriz-hax/model-stead`. M8 review passed; the Runtime Management Extension remains deferred pending its own approved design/specification. See [milestone index](../milestones.md).
