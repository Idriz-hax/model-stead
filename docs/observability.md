# Router observability

`GET /metrics` exposes Prometheus text-format metrics. It remains protected by an administrator API key; scrape with an API key whose resolved role is `admin`. The endpoint is available whether the inference gateway is enabled or disabled. Existing scheduler metrics are retained.

## Inference metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `router_inference_requests_total` | Counter | `endpoint`, `outcome` | Terminal outcomes for fixed inference POST endpoints. |
| `router_inference_request_duration_seconds` | Histogram | none | Time from router handler entry through terminal return, including request upload, admission/queueing, and response streaming. This is router-observed duration, not model-generation latency. |
| `router_inference_queue_wait_seconds` | Histogram | none | Time spent in `Scheduler.Submit`, including immediate lease acquisition or a terminal queue result. Requests rejected before submission are not observed. |

Histogram buckets are cumulative and use boundaries `0.01`, `0.05`, `0.1`, `0.5`, `1`, `2.5`, `5`, `10`, `30`, and `60` seconds, plus `+Inf`; exposition includes `_bucket`, `_sum`, and `_count`.

Endpoint labels are restricted to `/v1/chat/completions`, `/v1/completions`, `/v1/responses`, `/v1/messages`, and `/v1/embeddings`. Outcomes are `completed`, `rejected`, `timed_out`, `cancelled`, `upstream_error`, or `interrupted`. The collector excludes `/v1/models` and `/v1/messages/count_tokens`. It does not label by model, identity, user, key, request ID, status code, or other client/upstream text.

`completed` means the proxy returned with a non-error upstream status and without observed cancellation. Upstream transport/HTTP errors use `upstream_error`; local/auth/admission/body-validation failures use `rejected`; queue deadline uses `timed_out`; observed cancellation uses `cancelled`; an incomplete/aborted handler uses `interrupted`.

## Admin Overview

The session-protected `/api/v1/admin/overview` response includes an `observability` object alongside the existing scheduler and live-request fields. Recent outcomes cover the current clock minute and the four preceding minute buckets; the response includes the window boundaries. Timing counts and sums are process-local since-restart totals; means are derived from sum/count and are unavailable until at least one sample exists.

Metrics are in memory only, reset on process restart, and do not use M6 request-history data. They are aggregate operational signals, not billing or capacity guarantees. Gateway-disabled operation reports no inference requests; scheduler metrics remain available as before.
