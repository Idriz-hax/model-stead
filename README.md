# model-stead

A self-hosted service for routing AI requests fairly across local inference backends. The core proxy supports selected OpenAI-compatible and Anthropic API routes, with authenticated dashboards, weighted fair scheduling, personal request history, TrueNAS deployment support, and router observability.

## Local development

Requirements: Go 1.23+, Node.js 22+, npm.

```sh
cp config.example.yaml config.yaml
# Set server.public_url to the exact HTTPS browser origin before first startup.
make frontend
go test ./...
go build -o bin/router ./cmd/router
./bin/router
```

The UI is served at the configured address; liveness and readiness endpoints are `/health` and `/ready`. On a new database, the router requires an exact HTTPS `server.public_url` and prints a one-time administrator setup code in startup logs. Use a TLS reverse proxy at that origin for browser setup; keep logs private until setup is complete. `CONFIG_FILE` selects a YAML configuration file (defaults to `config.yaml`). `make frontend` copies the generated Vite assets into the Go embed directory; these generated files are ignored by Git.

## Docker

```sh
docker build -t model-stead .
docker run --rm -p 8080:8080 -v model-stead-data:/app/data model-stead
```

Container defaults use `/app/config.example.yaml` and write SQLite data to `/app/data/router.db`. The image healthcheck calls the executable's `healthcheck` mode against `/ready`. For deployment-specific settings, mount a config file and set `CONFIG_FILE`; ensure the database directory is writable by the non-root container user.

For TrueNAS SCALE 25.10 Custom App deployment, see the [operator guide](docs/deployment/truenas-scale.md) and [Compose template](deploy/truenas/compose.yaml). Application defaults keep the gateway and scheduled backups disabled; the TrueNAS-specific example opts into scheduled backups and keeps the gateway disabled. Compose defaults to `ghcr.io/idriz-hax/model-stead:latest`; set `ROUTER_IMAGE` to pin a release tag. Configure `ROUTER_HOST_PORT` (a free host TCP port) independently of `ROUTER_CONTAINER_PORT` (default 8080, shared by listener and healthcheck). Set `ROUTER_CONFIG_PATH`, `ROUTER_DATASET_PATH`, and `ROUTER_BACKUP_DATASET_PATH` to operator-selected host paths. Container mount points stay `/app/config.yaml`, `/app/data`, and `/app/backups`; database, backup, and spool paths in config should use the corresponding persistent mount. The router includes one-shot `backup` and offline `restore <path>` CLI commands.

Published stable GitHub releases publish `ghcr.io/idriz-hax/model-stead:<version>` and `:latest` for `linux/amd64` (for example, replace `<version>` below with the release's `vMAJOR.MINOR.PATCH` tag):

```sh
docker pull ghcr.io/idriz-hax/model-stead:<version>
docker pull ghcr.io/idriz-hax/model-stead:latest
```

After the first image is published, change the GHCR package visibility to **Public** in GitHub package settings to allow unauthenticated pulls. No registry credentials are needed in Compose.

## Configuration

See [`config.example.yaml`](config.example.yaml). Server address, SQLite path, and log level are required. Supported levels: trace, debug, info, warn, error. The embedded SQLite driver (`modernc.org/sqlite`) is pure Go, so the service builds with `CGO_ENABLED=0`; this avoids requiring a C toolchain in deployment.

The proxy is disabled by default. To opt in, set `gateway.enabled: true` and a valid HTTP(S) `gateway.base_url`; `LLAMA_SWAP_API_KEY` is sent upstream as a Bearer credential. **Do not expose the gateway to untrusted clients unless API-key auth is enabled and the deployment is appropriately protected.** Request bodies are size-limited and spooled to disk with a global in-flight byte budget; budget exhaustion returns HTTP 429. Spools use mode-0600 files in `gateway.spool_dir`, which is created mode 0700. When unset, the directory defaults to `spool` beside the configured database. Remove this directory's contents on clean shutdown/maintenance; active request files are removed when their request body closes, including cancellation. HTTP header reads time out after 10 seconds, request reads after 10 minutes, and idle keep-alive connections after 2 minutes. There is intentionally no response write timeout, so long-running inference streams are not cut off by a fixed deadline.

Supported pass-through routes are `GET /v1/models` and `POST /v1/chat/completions`, `/v1/completions`, `/v1/responses`, `/v1/messages`, `/v1/messages/count_tokens`, and `/v1/embeddings`. Requests and responses are not translated between provider protocols.

## Authentication (Milestone 3)

On a database with no administrator, open the app through the exact HTTPS origin in `server.public_url`, enter the one-time code printed in startup logs, and choose the username, display name, and password in the browser. The code exists only in process memory and is replaced after a restart while setup is still pending; restrict access to the app and keep startup logs private until setup completes. After the first administrator is created, setup is disabled and normal login continues across restarts. Legacy `auth.admin.*` YAML bootstrap keys are ignored and never create an account. Serve login over HTTPS: the session cookie is always `Secure`, `HttpOnly`, `SameSite=Lax`, and `Path=/`.

Provision a client API key locally after bootstrapping a user: `./bin/router create-api-key <username>` (or the equivalent binary path). The plaintext key is printed once; save it securely. Send it as `Authorization: Bearer <key>` or `X-API-Key`. Only the SHA-256 digest is stored. A supplied invalid/revoked/expired key is rejected and never falls back to IP-based identity.

IP fallback is off by default. Enable it only on a trusted network and configure `auth.trusted_proxy_cidrs` for the immediate proxy peers; forwarded client addresses are ignored from untrusted peers. Anonymous identity is off by default and requires an allowlisted CIDR plus a persistent secret environment variable of at least 32 bytes. To bound database growth, the anonymous identity table is capped at `auth.anonymous.max_identities` (default 100,000); new identities are rejected with HTTP 429 at capacity. IP-based identity is network-based, not proof of an individual person.

## Fair scheduling (Milestone 4)

Every inference POST acquires an in-memory scheduler lease before it is sent upstream; `GET /v1/models` is metadata and does not consume an inference slot. Defaults are four global slots, weighted round-robin admission, per-identity queue 100, global queue 500, maximum wait 30 minutes, and contended active limit 2. A single identity may use idle capacity. User scheduling weights and limits come from the existing user record; identities without a user record use scheduler defaults. Queued requests are canceled with their client context and timed out at `scheduler.max_wait`. `GET /metrics` exposes aggregate capacity, active, reservations, queue, and dispatch counters without identity labels and requires an administrator API key. The live scheduler and request registry are in-memory; active/queued state is not restored after restart.

## Administrator dashboard (Milestone 5)

Sign in at the embedded web app using the administrator account created during first-run setup. Dashboard sessions use the secure browser cookie and exact `server.public_url` Origin/CSRF checks for mutations. API keys authenticate inference clients, not dashboard administration. Admin APIs are under `/api/v1/admin/` and require an active admin session; the gateway remains disabled by default.

The dashboard provides Overview, live Requests, Users, Devices, API keys, Scheduler, and Audit views. Live updates use an admin-session SSE stream with a five-second refresh fallback. Current request records are held only in memory and contain bounded routing metadata; prompts and request bodies are never shown or retained. Admins can cancel queued requests immediately and must confirm interruption of active upstream inference. Clients can also cancel their own current inference by closing its connection.

Create users with a temporary password (minimum 12 characters); the user must change it at first login. Passwords are stored as Argon2id hashes. Disabling a user preserves the account and audit history, immediately revokes its API keys and browser sessions, and cancels its queued/active work. The final active administrator cannot be disabled or demoted. Device mappings are individual IPv4/IPv6 addresses; marking one untrusted preserves the record but removes it from future device identity resolution.

Dashboard-created API keys are shown once. Save the secret when created; only its prefix/metadata and a SHA-256 digest are retained. Revocation blocks future authentication. The local `create-api-key` command remains available for provisioning.

Scheduler settings are stored in SQLite after the first startup: validated YAML values seed the settings row only when it does not exist, and persisted values take precedence on later restarts. Supported bounds are capacity 1–64, default/per-user weight 0.1–100, global/per-user queue limits 1–10,000, max wait 1 second–24 hours, and cancellation grace 1 second–5 minutes. Durations must be whole seconds. Lowering capacity never interrupts current leases; new dispatch waits until usage is below the updated limit.

Audit events are append-only and actor-attributed. They record redacted administrator changes and omit passwords/hashes, API-key secrets/hashes, session/CSRF tokens, prompts, and request bodies. Keep the SQLite database and its backups protected as they contain account and operational metadata.

## Personal dashboard (Milestone 6)

Signed-in users can open My dashboard for their own 30-day provider-reported usage and request history. Administrators can open the same personal view for their own requests; it does not grant access to other users' history. These endpoints require an active browser session; API keys and anonymous identities do not provide personal history. Forced-password-change sessions must complete onboarding before accessing the dashboard. Cancellation is limited to live requests owned by the current session, and active cancellation confirms that upstream inference and the client response will be interrupted.

Request history is automatically purged after 30 days. It stores bounded routing/lifecycle metadata and provider-reported token counts only; prompts, request/response bodies, credentials, and arbitrary headers are never stored. Token values may be partial or unavailable when providers omit usage, responses are encoded, or the bounded JSON/SSE observer cannot safely parse them. Unavailable counts are not presented as zero, and no cost estimate is inferred.

## Router observability (Milestone 8)

The authenticated `/metrics` endpoint retains scheduler metrics and exposes bounded inference outcome counters plus router-observed request-duration and scheduler-wait histograms. The admin Overview adds recent outcome counts and timing means. Metrics are in-memory and reset on process restart; labels never include identities, model names, request IDs, prompts, or credentials. See [observability details](docs/observability.md) for metric labels, buckets, authorization, window definitions, and limitations. Read-only llama-swap status is separately opt-in; runtime controls remain deferred and llama-swap retains runtime ownership.

Read-only runtime status is configured separately with `runtime_management.enabled`, `runtime_management.base_url`, and `runtime_management.api_key_env` (default `RUNTIME_MANAGEMENT_API_KEY`). The adapter contract is pinned to llama-swap v269 (`9fbc47ce96d10160e95994c7a26513c972201a74`). Use HTTPS for non-loopback URLs; HTTP is accepted only for localhost/loopback so the Bearer key is not sent in cleartext over a network. Enable llama-swap API-key authentication and network/firewall isolation. Status is intended for active administrator sessions only.

## Development notes

- Current milestone statuses and links to each milestone record: [docs/milestones.md](docs/milestones.md).
- `make test`, `make build`, and `make frontend` are convenience targets; `make build` builds and synchronizes frontend assets first.
- Schema changes use forward-only Goose migrations in `internal/database/migrations`.
- Readiness is exposed only after configuration validation and database migration initialization have succeeded; it does not continuously probe the database afterward.
