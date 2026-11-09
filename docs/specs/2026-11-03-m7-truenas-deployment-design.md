# M7 TrueNAS SCALE Deployment and Backup Design

**Status:** Implementation delivered; original design-review approval is not recorded here  
**Date:** 2026-11-03  
**Scope:** M7 only: deployment template and SQLite backup/restore integration for TrueNAS SCALE 25.10.

## Goal

Make AI Fairness Router straightforward to deploy as a TrueNAS SCALE 25.10 Custom App while preserving the existing Docker image security posture and gateway-disabled default. Provide configurable scheduled, online-consistent SQLite backups and a safe offline restore path.

## Approved context and assumptions

- Target TrueNAS SCALE 25.10 and its Custom App YAML / Docker Compose interface.
- `llama-swap` already runs as a separate TrueNAS App/container on the same host and is not to be modified by M7.
- The router reaches it using the TrueNAS host/node LAN address and the host-published port. The router uses the default/automatic bridge; it does not use host networking or MACVLAN.
- Container image distribution is intentionally undecided. The deployment template will contain a conspicuous replaceable image reference; ask the user to select the image source after the app implementation is complete.
- TrueNAS itself is unavailable in this development environment. Documentation and container-level checks cannot claim a tested installation on hardware.

## Deployment artifact

- Add a TrueNAS 25.10 Custom App Compose YAML template and operator guide.
- Publish container port 8080 on a configurable host port and retain the image's non-root execution, healthcheck, and `CONFIG_FILE` behavior.
- Bind a dedicated, operator-created TrueNAS dataset to `/app/data` for SQLite and other durable application state. Document that its ACL/ownership must permit the image's non-root UID/GID (65532:65532) to write.
- Bind a separate, operator-created dataset to the configured backup destination (example container path `/app/backups`). Do not store scheduled backup files beside the live database by default.
- Configure `gateway.base_url` to `http://<truenas-node-address>:<llama-swap-host-published-port>` when explicitly enabling the router gateway. Keep `gateway.enabled` false in shipped defaults and in the template unless the operator opts in. Do not add a llama-swap service, change its configuration, or expose an unauthenticated route.
- Keep TrueNAS paths, host ports, public origin, and secrets as operator-provided placeholders. Never commit a password, API key, or other credential.

## Scheduled SQLite backups

- Add an optional scheduler configuration with an explicit enable flag, configurable backup directory, configurable interval, and configurable maximum age and maximum count.
- Proposed safe defaults: backups disabled unless explicitly enabled; when enabled, interval defaults to 24 hours, maximum age to 30 days, and maximum count to 14. The operator may disable either one of the two retention limits, but at least one limit must remain enabled.
- On startup, reject invalid enabled-backup configuration clearly: missing/unwritable destination, non-positive interval, or both retention limits disabled. Do not silently run an unbounded backup schedule.
- Use SQLite's online backup facility to create a transactionally consistent standalone database image while the service continues handling requests. A scheduled backup must not pause proxying or block request lifecycle persistence.
- Write to a unique temporary file in the destination directory, finish and validate it (including SQLite integrity check), set restrictive file permissions, then atomically rename it to a timestamped final name. A failed/incomplete attempt must leave prior valid backups untouched and must not affect inference or service readiness.
- Apply retention only after a new backup has passed validation and been atomically published. Delete backups that exceed either configured bound while always retaining the newest valid backup. Emit bounded, generic success/failure log events; do not add metrics or status endpoints for M7.
- The backup contains the SQLite database only. It excludes deployment configuration, environment variables/secrets, spool files, and arbitrary filesystem contents. The database itself contains sensitive account, key/session metadata, admin audit, and personal history; document restrictive dataset access and recommend TrueNAS dataset encryption and off-host replication.
- Hold an advisory process lock associated with the database path for the full serving-process lifetime. A second router process using the same database path must fail startup clearly; this single-instance-per-database rule lets offline restore reliably reject a live but currently idle router. One-shot online backup does not take the serving lock and may run while the app serves requests.
- Add an operator-triggered one-shot CLI backup operation using the same safe online backup implementation so operators can verify a destination before relying on the scheduled job.

## Restore

- Add an offline CLI restore operation that reads the configured database path and a selected backup file.
- Acquire the same advisory process lock non-blockingly before restore. If the router is running or another restore holds the lock, reject without changing the source or target. The operator must stop the router App before restoring.
- Validate the source database and schema before replacing anything. Stage the replacement beside the target, preserve the current database as a recoverable pre-restore copy, then atomically replace the target where the filesystem supports it. On any failure, leave the current database intact and return a non-zero CLI status.
- The operator guide must explain restoring the database dataset, configuring the original runtime secrets separately, starting the service, and verifying `/readyz` and owner/admin access. Restoring a database does not restore TrueNAS configuration, deployment YAML, secrets, inference data, or spool contents.

## Security and compatibility boundaries

- Never copy credentials or config into backup filenames, logs, or backup metadata.
- Backup and restore paths are trusted operator configuration, not HTTP/API input. Validate paths, avoid following unexpected symlinks where practical, and use restrictive permissions.
- Keep the application gateway disabled by default; do not weaken session/CSRF/authentication controls or add deployment management endpoints.
- Do not change llama-swap ownership/configuration, add M8 observability, or modify M6 retention/accounting behavior.
- Existing deployments without the backup section continue to start with no scheduled backup job. Database migrations and application startup behavior remain unchanged.

## Verification

- Unit/integration tests for online backup under concurrent writes, integrity validation, restrictive permissions, retention by age/count/combined policy, failed-write safety, and restore success/corrupt-source/active-target safety.
- Configuration tests for defaults, opt-in, invalid intervals/paths, and bounded retention requirements.
- Verify the Compose YAML syntax and operator placeholders; document that syntax validation is not a TrueNAS installation test.
- Run the project's full gates: `go test ./...`, `go test -race ./...`, `go build ./cmd/router`, `go mod verify`, frontend build, Docker image build, and container smoke checks.
- On target hardware, manually verify SCALE 25.10 Custom App deployment, dataset permissions, access to the existing llama-swap published port over the automatic bridge, scheduled backup creation/retention, restore, and `/healthz`/`/readyz`.

## Alternatives considered

1. **TrueNAS snapshots/replication only:** least application code and useful for disaster recovery, but does not meet the requested in-app scheduled online SQLite backup and restore workflow.
2. **In-app SQLite online backup scheduler plus TrueNAS dataset replication (selected):** provides a consistent database copy without stopping inference, with the operational complexity of scheduling, retention, and restore tooling.
3. **Backup sidecar:** keeps backup logic out of the router but adds another image/service and coordination around SQLite; not selected.

## Official TrueNAS references (checked 2026-11-03)

- [Custom App Screens — TrueNAS SCALE 25.10](https://www.truenas.com/docs/scale/25.10/scaleuireference/apps/installcustomappscreens/): install via YAML with Docker Compose, host-path mounts, container/host port mapping, and host-networking behavior.
- [Containers — TrueNAS SCALE 25.10](https://www.truenas.com/docs/scale/25.10/scaletutorials/containers/): automatic bridge networking provides communication between containers and the TrueNAS host; MACVLAN on the same physical interface cannot directly communicate with the host.
- [Adding and Managing Datasets — TrueNAS SCALE 25.10](https://www.truenas.com/docs/scale/25.10/scaletutorials/datasets/datasetsscale/): recommends separate per-app datasets for application data.
- [Backing Up TrueNAS — TrueNAS SCALE 25.10](https://www.truenas.com/docs/scale/25.10/gettingstarted/configure/setupbackupscale/): system configuration export and data backup are separate responsibilities; data can be protected using replication, cloud sync, or rsync.
- [Replication Tasks — TrueNAS SCALE 25.10](https://www.truenas.com/docs/scale/25.10/scaletutorials/dataprotection/replication/): dataset snapshots can be replicated locally or remotely.
