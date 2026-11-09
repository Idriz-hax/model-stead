# M7 TrueNAS Deployment Implementation Plan

> **Archive status:** M7 implementation is delivered and reviewed; TrueNAS hardware validation remains outstanding. See [milestone index](../milestones.md).

**Goal:** Deploy model-stead as a TrueNAS SCALE 25.10 Custom App and provide scheduled online SQLite backups plus safe offline restore.

**Status:** Implemented, reviewed, and verified; TrueNAS hardware validation remains.

**Architecture:** Keep the existing non-root single-container image, default-disabled gateway, and SQLite database path. Add optional configuration-driven backup scheduling around `modernc.org/sqlite`'s online backup API, isolate backup files on a separately mounted path, and provide operator CLI create/restore commands. Add a TrueNAS Compose template that connects to the existing llama-swap App over the TrueNAS host address and published port.

**Tech Stack:** Go 1.23, `database/sql`, `modernc.org/sqlite` v1.34.5, existing YAML config loader, Docker Compose YAML, TrueNAS SCALE 25.10 Custom Apps.

---

## File and responsibility map

| Path | Responsibility |
|---|---|
| `internal/config/config.go` | Parse and validate optional backup settings. |
| `config.example.yaml` | Show safe opt-in backup defaults and gateway remains disabled. |
| `internal/database/process_lock.go` | Advisory lock held by one serving process per database path. |
| `internal/database/migrate.go` | Expose current schema version and add an existing-database open helper for CLI backup without repeating migrations. |
| `go.mod`, `go.sum` | Promote `golang.org/x/sys` to a direct dependency if required for the advisory lock implementation. |
| `internal/backup/backup.go` | Online SQLite backup, validation, restore staging, and safe replace. |
| `internal/backup/retention.go` | Age/count retention of only managed backup filenames. |
| `internal/backup/scheduler.go` | Cancellable interval loop; never owns the HTTP server lifecycle. |
| `cmd/router/main.go` | One-shot backup/restore command dispatch and scheduler start/stop. |
| `internal/backup/*_test.go`, `internal/config/config_test.go`, `cmd/router/*_test.go` | Backup correctness, retention, config, CLI and lifecycle tests. |
| `deploy/truenas/compose.yaml` | SCALE Custom App Compose template with replaceable image and operator paths. |
| `deploy/truenas/config.example.yaml` | M7 deployment config example with gateway off and scheduled backup explicitly enabled. |
| `docs/deployment/truenas-scale.md` | Dataset permissions, networking, install, backup, restore and hardware checks. |
| `README.md` | Link the M7 deployment guide and summarize safe defaults. |

The workspace has no Git metadata; do not create worktrees or add commit steps.

### Task 1: Backup configuration and validation

**Files:** `internal/config/config.go`, `internal/config/config_test.go`, `config.example.yaml`.

- [x] **Step 1: Add failing config tests** for omitted settings (disabled), enabled defaults (24h/30d/14), custom interval/directory, zeroing either one retention bound, and invalid enabled combinations (empty directory, non-positive interval, negative bounds, or both retention bounds disabled).
- [ ] **Step 2: Run the config tests red:** not observed before implementation because the host Go toolchain was unavailable; tests passed afterward in the Go Docker container (see Task 6).
- [x] **Step 3: Add `Backup` config fields**: `Enabled bool`, `Directory string`, `Interval string`, `MaxAge string`, `MaxBackups int`; defaults are disabled, `24h`, `720h`, and `14`. Parse max-age as a Go duration. When enabled, require a non-empty directory, positive interval, non-negative max age/count, and at least one active retention bound. Do not create the backup directory here; the backup service owns filesystem checks.
- [x] **Step 4: Add YAML example and validate:** add an explicitly commented `backup` section to `config.example.yaml`; leave `gateway.enabled: false`. The full suite passed in Docker.

### Task 2: Safe online backup and restore engine

**Files:** Create `internal/backup/backup.go`, `internal/backup/backup_test.go`; add current schema-version constant in `internal/database/migrate.go`.

- [x] **Step 1: Write failing online-backup tests** using a temporary SQLite DB opened with `database.Open`: create a row, back it up, open the output independently, and assert the row exists. Add a concurrent writer during backup and assert the resulting database passes `PRAGMA integrity_check` and contains a transactionally valid state.
- [ ] **Step 2: Run red:** not observed before implementation because the host Go toolchain was unavailable; tests passed afterward in the Go Docker container (see Task 6).
- [x] **Step 3: Implement backup primitive:** acquire one `*sql.Conn`, use `Conn.Raw` and assert the driver implements `interface { NewBackup(string) (*sqlite.Backup, error) }`; copy in bounded page batches so cancellation can be checked between calls to `Step`, always `Finish`, and propagate step/finish errors. Build at a temporary path in the configured destination directory, chmod 0600, validate with a separate SQLite connection and `PRAGMA integrity_check`, then atomically rename to a UTC timestamped `.db` file. Remove only this attempt's temp file on failure.
- [x] **Step 4: Add restore tests first**: valid backup restores into a fresh target; invalid/corrupt or incompatible-schema backup leaves existing target bytes and data unchanged; staging/rename failure leaves the original target intact. The CLI's process-lock rejection test belongs to Task 4.
- [x] **Step 5: Implement restore staging** with `NewRestore` through `Conn.Raw` into a staged database, then run integrity and required-schema checks before replacement. `backup.Restore` acquires the database process lock before staging. Preserve the existing database and any `-wal`/`-shm` sidecars as a uniquely named pre-restore set before atomic replacement; roll the set back on any replacement failure.
- [x] **Step 6: Verify engine behavior:** gofmt and the full Go test suite passed in the Go Docker container.

### Task 3: Retention, scheduler, and database process lock

**Files:** Create `internal/backup/retention.go`, `internal/backup/scheduler.go`, their tests, and `internal/database/process_lock.go` with tests; modify `cmd/router/main.go` for process-lock and worker lifecycle.

- [x] **Step 1: Test the process lock:** two independently opened locks for the same DB path must make the second acquisition fail promptly; a lock on another path succeeds; closing the first permits reacquisition. Use a persistent `0600` `<database-path>.lock` file and advisory `LOCK_EX|LOCK_NB`; never unlink it on close, because another process may already have opened that inode.
- [x] **Step 2: Implement the process lock** with `golang.org/x/sys/unix.Flock` behind `AcquireProcessLock(path) (*ProcessLock, error)` and idempotent `Close`; `golang.org/x/sys` is a direct module dependency. The serving process acquires it before `database.Open`/migrations and holds it until server exit; a second instance exits with a generic single-instance error.
- [x] **Step 3: Test retention** using timestamped managed backup files plus unrelated files, directories, symlinks, and `.tmp` files. Verify age-only, count-only, combined (age OR count exceeded), and “always retain newest valid backup”; verify unknown files are never removed.
- [x] **Step 4: Implement retention** to enumerate only regular files matching the exact router backup filename pattern, validate candidates, sort newest-first by the embedded UTC timestamp, keep newest valid, and remove files older than max age or beyond max count. Run retention only after a newly validated backup is published.
- [x] **Step 5: Test the scheduler** with a short interval and a fake backup function: no work before the first interval, one call per tick, no overlapping calls, failures do not stop later attempts, and context cancellation ends the loop.
- [x] **Step 6: Implement cancellable backup service**: create and verify destination write access when enabled; use a ticker and execute one backup per interval (manual CLI provides immediate backup); run retention only after success; log bounded generic success/failure events without leaking paths or secrets. Check context between SQLite backup page batches so shutdown removes the temporary file. No startup backup.
- [x] **Step 7: Wire and verify**: start the service only when config enables it; stop and join it when `ListenAndServe` returns. Wire the database lock into normal server startup but not `create-api-key` or online backup. Focused/full tests passed in the Go Docker container.

### Task 4: Operator backup and restore commands

**Files:** Modify `cmd/router/main.go` and `internal/database/migrate.go`; add CLI tests in `cmd/router/main_test.go` or a focused CLI helper test file.

- [x] **Step 1: Add CLI tests**: `router backup` creates an immediate verified backup in configured destination; `router restore <backup-file>` rejects while process lock is held, restores valid DB when stopped, rejects corrupt input, and preserves pre-restore DB on failure. Malformed command argument counts are rejected; existing `healthcheck` and `create-api-key` dispatch remains intact.
- [x] **Step 2: Add `database.OpenExisting(path)`** using the same SQLite DSN/pragmas as `Open` but without Goose migration. Use this for `backup` so a CLI backup concurrent with the server does not attempt schema migration.
- [x] **Step 3: Dispatch `backup` and `restore` before `database.Open`**: load config; backup opens the existing DB and invokes the same backup-plus-retention implementation; restore calls `backup.Restore`, which owns process-lock acquisition and staged replacement (do not acquire the lock separately). Print concise actionable failures to local stderr without secrets; return non-zero on error.
- [x] **Step 4: Verify backup/restore commands**: focused and full Go test suites passed in the Go Docker container.

### Task 5: TrueNAS Compose template and operator guide

**Files:** Create `deploy/truenas/compose.yaml` and `docs/deployment/truenas-scale.md`; modify `README.md`.

- [x] **Step 1: Add `deploy/truenas/config.example.yaml`** with required settings, `/app/data/router.db`, backup path `/app/backups`, scheduled backup defaults, gateway disabled, HTTPS public-origin placeholder, and gateway URL to llama-swap's host-published port if deliberately enabled. Use no secret values.
- [x] **Step 2: Add the Compose YAML** for one router service: replaceable `image` placeholder, restart policy, `CONFIG_FILE`, read-only config bind mount, configurable host port (loopback by default), dedicated host-path datasets, and operator-entered secret environment variables. No privileged mode, host networking, MACVLAN, or llama-swap service.
- [x] **Step 3: Validate the template** with sample interpolation values using `docker compose -f deploy/truenas/compose.yaml config --quiet`; syntax/config validation passed.
- [x] **Step 4: Write the operator guide** covering datasets, UID/GID 65532, config/secrets, Automatic Bridge, existing llama-swap host-published port, secure HTTPS reverse-proxy options, and image-source placeholder.
- [x] **Step 5: Document recovery**: describe the implemented `router backup` and `router restore <path>` flows, retention semantics, stopping the app before restore, post-restore checks, database-only contents, encryption, and off-host replication.
- [x] **Step 6: Link documentation** from README, preserve disabled application defaults, and verify no literal credentials or pool-specific paths were added.

### Task 6: Full verification and review

**Files:** Final inspection of `config.example.yaml`, `cmd/router`, `internal/config`, `internal/database`, `internal/backup`, `deploy/truenas`, `docs/deployment`, and `README.md`.

- [x] **Step 1: Run tests/build/module verification**: `docker run --rm -v "$PWD:/src" -w /src golang:1.23-alpine sh -c 'go test ./... -count=1 && go build ./cmd/router && go mod verify'`; all passed.
- [x] **Step 2: Run race tests**: `docker run --rm -v "$PWD:/src" -w /src golang:1.23 sh -c 'go test -race ./... -count=1'`; passed.
- [x] **Step 3: Build and smoke the container**: `docker build -t afr-m7-smoke .` passed. Container smoke verified `/healthz`, `/readyz`, embedded `/`, anonymous APIs return 401, gateway returns 404, scheduled and manual backup, live-restore rejection, stopped restore, and restart using Linux Docker-managed volumes. A macOS Docker Desktop host bind mount did not honor cross-process `flock`; this limitation is documented and the target TrueNAS dataset must be checked on hardware.
- [x] **Step 4: Review only after tests pass**: spec, security, code-quality, and completion audits passed. TrueNAS host UI installation and dataset lock behavior remain user-hardware verification, not claims from local tests.

## Execution notes

- `docker compose -f deploy/truenas/compose.yaml config --quiet` passed with sample interpolation values.
- `cd frontend && npm run build` passed. Go formatting was applied with `gofmt` in `golang:1.23-alpine`.
- The TDD red runs were authored but not observed before implementation because Go was absent on the host; the completed tests and race suite passed afterward in Docker.
- Container image distribution remains deliberately deferred until the user completes their own release testing; do not choose or publish an image source yet. TrueNAS SCALE 25.10 installation has not been run on hardware.
