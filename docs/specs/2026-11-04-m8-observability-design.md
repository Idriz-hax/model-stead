# Milestone 8 — Router Observability Design

**Status:** Approved — implementation authorized  
**Implementation:** Complete; verification and specialist reviews passed (2026-11-04)  
**Date:** 2026-11-04  
**Scope:** M8 only. Preserve M1–M7. The Runtime Management Extension remains follow-on work.

## Goal

Extend the router's existing Prometheus-compatible metrics and administrator dashboard with useful operational visibility into inference request outcomes and timing, without changing routing or fairness behavior.

## Existing foundation

- `GET /metrics` currently exposes scheduler capacity, active requests, reservations, queued requests, and total dispatches.
- `/metrics` is protected by an administrator API key.
- The admin dashboard already shows live scheduler/request state and receives SSE updates.
- M6 persists bounded user-owned request lifecycle metadata and provider-reported token counts; M8 must not use this private history as an unbounded metrics-label source.

## Approved scope

- Add one process-local bounded collector shared by `/metrics` and the admin Overview API. Keep counters and fixed-bucket histograms in memory; persist no telemetry and retain no per-request records.
- Count only fixed inference POST routes and use fixed endpoint/outcome labels. Never label by user, API key, request ID, model name, prompt, status text, or arbitrary upstream value.
- Keep a recent outcome window using the current minute and four preceding minute buckets. Identify this exact window; do not claim it is an exact sliding five-minute window.
- Measure request duration from eligible inference handler entry through terminal handler return/unwind, including upload, admission, queueing and streaming. Measure queue wait from scheduler submission until lease or terminal queue result. These are router-observed timings, not model-generation latency.
- Expose Prometheus histograms with `_bucket`, `_sum`, `_count`, cumulative fixed buckets and `+Inf`.
- Preserve existing scheduler metrics and endpoint authorization; keep metrics available when the gateway is disabled with zero-valued or no-request series as appropriate.
- Extend the existing admin Overview response with an `observability` object sourced directly from the shared collector, never by scraping/parsing `/metrics`. Include process start time, gateway enabled state, recent outcome counts and since-restart duration/queue-wait counts and sums. Derive means; report no-data when count is zero.
- Instrument inference POST attempts independently of optional M6 history writes, including observable local auth/admission rejections. Exclude unrelated paths, wrong methods and model discovery. Record exactly one terminal outcome per attempt, preserving rejection, timeout, cancellation, upstream failure and interruption semantics.
- Surface concise aggregate signals in the existing administrator Overview; distinguish loading, no-samples, stale and unavailable/error states. A valid zero recent count is distinct from missing samples.
- Document metric names, labels, units, reset behavior, authorization, and scrape usage.
- Add focused Go tests for lifecycle outcomes/timing, metric format/cardinality safety, and recent-window rollover. Verify dashboard loading, no-samples, stale and unavailable states with browser smoke checks; do not add a new frontend test dependency solely for this compact panel.

## Constraints and non-goals

- No change to scheduling policy, admission capacity, inference routing, or gateway default (still disabled).
- No database migration, Prometheus dependency, new service, background sampler, or new dependencies; metrics are process-local and reset on restart.
- No GPU telemetry, llama-swap discovery/control, model-specific metrics, percentile calculations, token aggregation, alerts, dashboards-as-a-service, or arbitrary metric labels.
- Do not expose user identities, request contents, secrets, or high-cardinality values through metrics.
- Do not claim end-to-end model latency; timing covers the router-observed request lifecycle and must be named/documented precisely.

## Acceptance criteria

1. Existing scheduler metric names/values and `/metrics` authorization remain compatible.
2. Completed, rejected, timed-out, canceled, and upstream-failed requests update bounded outcome metrics exactly once where observable.
3. Request-duration and queue-wait histograms observe valid non-negative durations with documented units and fixed buckets.
4. Metric labels are allowlisted and bounded regardless of client-supplied model names, identities, headers, or request IDs.
5. Gateway-disabled operation remains safe and reports no misleading inference activity.
6. Administrator Overview communicates current scheduler state, outcomes in the current-plus-four-prior-minute window, and since-restart timing means without duplicating M6 private history or showing false zeroes during loading/failure.
7. Automated Go tests cover metric exposition, auth, cardinality boundaries and lifecycle correctness; browser smoke covers UI states; docs describe operational limits.
8. Existing full gates in `AGENTS.md` pass; no changes to M1–M7 behavior.

## Verification

Focused Go tests cover the metrics collector and all observable lifecycle paths; frontend build and focused browser checks cover dashboard states and responsive/accessibility behavior. Before completion run `go test ./...`, `go build ./cmd/router`, and `cd frontend && npm run build` as required by `AGENTS.md`.
