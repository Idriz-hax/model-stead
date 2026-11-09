# model-stead Architecture

model-stead is a single Go service with a static React administration app embedded in the binary. Startup validates YAML, opens SQLite, applies forward-only Goose migrations, initializes the fair scheduler and its bounded process-local metrics collector, then serves HTTP.

## Request path

Inference routes are opt-in and disabled by default. When enabled, the router authenticates the client, validates and spools the bounded request body, acquires a scheduler lease, then forwards the request to the configured OpenAI-compatible or Anthropic-compatible upstream. It preserves the upstream response stream and credential boundary. `/v1/models` is metadata and does not consume a scheduler lease. User-owned request history is written separately and must not be required for inference or aggregate metrics.

## Administration and persistence

Dashboard authentication uses browser sessions; admin APIs apply role checks and CSRF/Origin validation for mutations. SQLite stores users, credentials metadata, scheduler settings, audit events, and user-owned request history. Scheduler leases, live request cancellation handles, SSE clients, and M8 aggregate observations are process-local and reset on restart. The static browser application is built by Vite and embedded from `web/dist`.

## Observability

The admin-session Overview reads a bounded snapshot directly from the in-memory observability collector. The admin-API-key-protected `/metrics` endpoint exports fixed-label inference outcome counters and fixed-bucket duration/queue-wait histograms alongside existing scheduler gauges/counters. No metric label contains user identity, model name, request ID, prompt, or credential. Timing means describe router-observed request handling, not model-generation latency. See [observability details](observability.md).

## Deployment boundary

The default deployment remains a lightweight, non-root, GPU-free router connected to an external inference service. llama-swap owns model scheduling and process lifecycle; M8 adds no llama-swap controls, GPU device access, or container lifecycle interface. TrueNAS deployment and backup guidance is in [the operator guide](deployment/truenas-scale.md).
