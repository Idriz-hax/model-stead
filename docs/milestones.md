# model-stead milestone index

This is the current status index. Dated design documents and implementation plans under `docs/specs/` and `docs/plans/` remain the detailed historical record. Do not treat unchecked boxes in archived plans as current work. M1–M4 predate the checked-in milestone-spec format; their summaries below are based on the project record and current README, not reconstructed specifications.

| Milestone | Status | Scope and record |
|---|---|---|
| M1 — Service foundation | Implemented | Go startup/configuration, SQLite/Goose, embedded static UI, health endpoints, Docker. No dedicated spec/plan found. |
| M2 — Opt-in inference proxy | Implemented | Selected OpenAI-compatible/Anthropic routes, streaming/cancellation, bounded spooling and upstream credential isolation. No dedicated spec/plan found. |
| M3 — Authentication and identity | Implemented | API keys, device/IP/anonymous identity, dashboard sessions, password security, CSRF/Origin protections and bootstrap. No dedicated spec/plan found. |
| M4 — Fair scheduling | Implemented | In-memory weighted fair scheduler, per-identity FIFO, bounded queues, leases, cancellation/timeouts, aggregate admin metrics. No dedicated spec/plan found. |
| M5 — Administrator dashboard | Implemented | [Design](specs/2026-11-03-m5-admin-dashboard-design.md) · [historical implementation plan](plans/2026-11-03-m5-admin-dashboard.md). |
| M6 — Personal dashboard/history | Implemented | [Design](specs/2026-11-03-m6-personal-dashboard-design.md) · [historical implementation plan](plans/2026-11-03-m6-personal-dashboard.md). |
| M7 — TrueNAS deployment/backup | Implemented; hardware validation outstanding | [Design and scope](specs/2026-11-03-m7-truenas-deployment-design.md) · [plan and verification limits](plans/2026-11-03-m7-truenas-deployment.md) · [operator guide](deployment/truenas-scale.md). The image reference remains a placeholder; TrueNAS installation has not been verified on hardware. |
| M8 — Router observability | Implemented and reviewed | [Approved design and acceptance criteria](specs/2026-11-04-m8-observability-design.md) · [completed plan](plans/2026-11-04-m8-observability.md) · [metric contract](observability.md). |

## Next staged milestone

The Runtime Management Extension (RME) is the next proposed milestone. Its read-only-first design is drafted at [the RME design spec](specs/2026-11-04-rme-read-only-runtime-status-design.md) and is awaiting user review; it is not yet approved for implementation. Preserve llama-swap as runtime owner and keep the gateway disabled by default.
