# TrueNAS SCALE 25.10 deployment

This guide provides a Custom App Compose template for the router using the public GHCR image by default. Set `ROUTER_IMAGE` to a specific release tag to pin it. TrueNAS hardware installation has not been validated by this project.

Stable GitHub releases publish `ghcr.io/idriz-hax/model-stead:<version>` and `:latest` for `linux/amd64`; use a `vMAJOR.MINOR.PATCH` release tag for a pinned version. Pull with `docker pull ghcr.io/idriz-hax/model-stead:<version>` or `docker pull ghcr.io/idriz-hax/model-stead:latest`. After the first image is published, change the package visibility to **Public** in GitHub package settings; otherwise unauthenticated pulls will not work. Do not put registry credentials in Compose.

## Prepare datasets and configuration

Choose operator-controlled host paths for the config file, data dataset, and separate backup dataset; do not use hardcoded pool paths. Do not put scheduled backups alongside the live database. Configure dataset ACLs/ownership so the container can write as UID/GID `65532:65532` (the image's non-root user). Restrict access: the database and backup files contain sensitive account, key/session metadata, audit records, and personal history. Enable dataset encryption where appropriate and configure snapshots plus replication to a separate/off-host destination; SQLite backups do not replace dataset protection.

Copy `deploy/truenas/config.example.yaml` to a protected host path and edit its public URL to match the exact HTTPS origin served by your TLS reverse proxy. Keep the config file readable by the container, but do not put secret values in it. On a new database, startup logs contain a one-time setup code; enter it in the browser setup form to create the first administrator. Restrict access to the app and keep logs private until setup completes. The code is process-local and changes after a restart while setup is pending. Existing databases with an administrator continue to use normal login. Older `auth.admin.*` bootstrap keys are ignored and no longer create accounts. `LLAMA_SWAP_API_KEY` is a variable name, not a credential to commit.

## Install the Custom App

1. In TrueNAS SCALE 25.10, open **Apps** and choose **Discover Apps** / **Custom App**, then select the YAML/Compose installation option.
2. Paste or upload `deploy/truenas/compose.yaml`.
3. The image defaults to `ghcr.io/idriz-hax/model-stead:latest`; set `ROUTER_IMAGE` to a specific release tag to pin the deployed version.
4. Set `ROUTER_HOST_PORT` to a free host TCP port and `ROUTER_CONFIG_PATH`, `ROUTER_DATASET_PATH`, and `ROUTER_BACKUP_DATASET_PATH` to the actual config/data/backup paths. Set `ROUTER_CONTAINER_PORT` only to change its default of 8080; it configures the app listener and Compose target together, and must be a valid port. Only the host-side port needs to avoid conflicts with other apps. The router binds to `127.0.0.1` on the TrueNAS host by default; set `ROUTER_BIND_ADDRESS` only if you have deliberately configured appropriate network/TLS protection.
5. Set any upstream secret environment values in the TrueNAS editor. Do not add credentials to the Compose file or config YAML.
6. Keep networking on the automatic bridge. Do not select host networking or MACVLAN. The template maps the selected host port to the container port and mounts the stable targets `/app/config.yaml`, `/app/data`, and `/app/backups`. Keep `database.path` inside `/app/data`, `backup.directory` inside `/app/backups`, and `gateway.spool_dir` inside `/app/data` unless you add another volume. It includes no llama-swap service.
7. Deploy and check `http://127.0.0.1:<router-host-port>/health` and `/ready` from the TrueNAS host, or check them through the configured HTTPS reverse proxy. Do not expose the login UI directly over plain HTTP. Complete first-run setup and login only through the protected HTTPS origin.

The application runs non-root as UID/GID 65532. If the dataset ACLs do not grant this identity write access, SQLite and scheduled backups will fail. Review TrueNAS app logs and correct dataset permissions rather than running the container privileged. The default loopback-only port binding avoids publishing an unencrypted login endpoint to the LAN; for remote browser access, place the app behind a TLS reverse proxy and set `server.public_url` to that exact HTTPS origin.

A reverse proxy running directly on the TrueNAS host can use the default `127.0.0.1` upstream. A proxy running in another container normally cannot reach the host's loopback listener; in that setup, bind `ROUTER_BIND_ADDRESS` to a private TrueNAS interface reachable by the proxy, then restrict that host port with firewall/network policy so only the proxy can connect. Avoid `0.0.0.0` unless access is equivalently restricted. Keep the browser-facing origin HTTPS regardless of the proxy-to-router transport.

## Optional inference gateway

The supplied deployment config keeps `gateway.enabled: false`; `/v1` proxy routes remain unavailable in this state. If intentionally opting in, set `gateway.enabled: true` and configure `gateway.base_url` as `http://<truenas-node-address>:<llama-swap-host-published-port>`. Obtain the actual node LAN address and published port from the existing llama-swap App. Automatic bridge networking can reach host-published services; do not use a container-only address. Do not expose the gateway to untrusted clients: enable API-key authentication and apply suitable network policy first. No llama-swap configuration is changed by this deployment.

Read-only runtime status is an independent optional integration (`runtime_management.enabled: false` by default), tested against llama-swap v269 (`9fbc47ce96d10160e95994c7a26513c972201a74`). To enable it, configure `runtime_management.base_url` for the existing llama-swap service and set `runtime_management.api_key_env` (default `RUNTIME_MANAGEMENT_API_KEY`) to the name of a TrueNAS environment secret. Use HTTPS for the non-loopback host-published URL: the router rejects plain HTTP to network hosts to protect the Bearer key in transit (HTTP is allowed only for localhost/loopback). Enable llama-swap API-key authentication and restrict network access with firewall policy; TLS does not replace network isolation. The router never manages the llama-swap process.

## Backups and recovery

Scheduled backups are enabled in this deployment config (unlike application defaults). The router is planned to create SQLite-consistent backups in `/app/backups` every 24 hours, retain backups up to 30 days and 14 files, and always preserve the newest valid backup. Retention applies when either limit is exceeded. Backups contain only the SQLite database—not config, environment secrets, spool files, inference data, or arbitrary files.

The `router backup` command creates an immediate validated online backup and applies retention. `router restore <path>` performs offline restore; stop the TrueNAS App first. Both commands are included in the current router build. Restore acquires the database process lock itself and refuses to proceed while the serving app holds it.

For restore: stop the TrueNAS App, run `router restore <path>` against the selected backup using the same image/config/data mounts, restart the app afterward, and verify `/ready`, admin login, and restored records. Restore/configure runtime secrets separately: restoring a database does not restore deployment YAML, TrueNAS configuration, secrets, spools, or inference data. Preserve the existing dataset and backups until recovery is verified.

## Hardware validation

Validate on the target SCALE 25.10 system before relying on this setup: app deployment, UID/GID dataset writes, automatic-bridge access to the existing llama-swap host-published port, health/readiness, backup creation/retention, restore, and admin access. The single-instance/restore exclusion relies on Linux advisory `flock` on the database dataset; verify on the target dataset that a restore attempt is rejected while the app is running. Some desktop-host bind mounts do not preserve these lock semantics. Local YAML validation is not a TrueNAS installation test. Verify backup replication and recovery independently.
