# Deployment Publishing and First-Run Administrator Setup

**Date:** 2026-11-09
**Status:** Proposed design for user review; implementation is not yet authorized
**Scope:** Proposed follow-up milestone after M8; the milestone index currently lists RME as the next unapproved proposal. Confirm sequencing before implementation.

## Context

M7 provides a TrueNAS SCALE 25.10 Compose template, but leaves the image source undecided and hardware validation outstanding. The project repository now contains the application, while the TrueNAS template still has an image placeholder. The template already exposes host-side bind address, host port, config file, data dataset, and backup dataset as operator inputs. Inside the app, the database path, backup directory, and optional gateway spool directory are configurable in YAML. The container listener and image healthcheck currently assume port 8080.

Administrator bootstrap currently takes username and display name from YAML and reads a password from a named environment secret. The password value is not stored in YAML, but the operator must prepare bootstrap credentials before startup. This proposal replaces that bootstrap path with a one-time browser setup flow and removes bootstrap credential fields from the supported YAML configuration.

## Goals

1. Let TrueNAS pull a versioned, public Docker/OCI image without manually importing an archive.
2. Let an operator independently choose the host-published port and the container listener port without making the healthcheck disagree with the app.
3. Make host paths for config, data, and backups explicit user inputs, and document the app-configurable database, backup, and spool paths.
4. Let the first administrator choose their account credentials through a one-time browser setup flow; do not store admin usernames or passwords in YAML.
5. Preserve existing installed databases and normal login behavior once an administrator exists; keep the inference gateway disabled by default.

## Design

### 1. Container image distribution

Publish a Docker/OCI image to GitHub Container Registry at `ghcr.io/idriz-hax/model-stead`. A GitHub Actions workflow runs only for a published, non-prerelease GitHub Release with a stable `vMAJOR.MINOR.PATCH` tag (for example, `v0.1.0`). It builds the `linux/amd64` image for the supported TrueNAS SCALE target and publishes both the version tag and `latest`. The TrueNAS Compose template uses the GHCR image by default and permits selecting another tag through a `ROUTER_IMAGE` input.

The workflow uses the built-in `GITHUB_TOKEN`, with only `contents: read` and `packages: write` permissions. Third-party actions are pinned to full commit SHAs. The image includes OCI source metadata. GitHub may create the first package as private; after the first publish, package visibility must be set to public in GitHub package settings so TrueNAS can pull it without registry credentials. A GitHub Release remains the human-facing version/release record; the image itself is pulled from GHCR rather than attached as a tarball.

Alternatives considered: a release-attached image archive requires downloading and importing into the TrueNAS host; publishing on every `main` commit adds a mutable development image that could be mistaken for a release. Release-triggered GHCR publishing is recommended for direct Compose pulls and stable version tags. Keeping the container port fixed would avoid app/config wiring, but the user requested both host and container ports be configurable.

### 2. Ports and storage paths

Expose three independent TrueNAS inputs:

- `ROUTER_BIND_ADDRESS`: host-side bind interface, retaining the safe loopback default.
- `ROUTER_HOST_PORT`: required host-published TCP port; the operator selects an unused port to avoid collisions.
- `ROUTER_CONTAINER_PORT`: container listener/target port, defaulting to 8080.

The same container-port input must configure the Go listener, the Compose target port, and the in-container `/ready` healthcheck. In the container, `ROUTER_CONTAINER_PORT` overrides only the port component of `server.address`; the configured bind host is preserved. It defaults to 8080. Validate it as a decimal TCP port in the range 1–65535. The Dockerfile's `EXPOSE 8080` is image metadata, not a runtime listener or host reservation; it may remain as the documented default. The operator guide explains that only the host port can collide with another host-published app, while the internal port is isolated by container networking.

Keep the host dataset/file path inputs in Compose: `ROUTER_CONFIG_PATH`, `ROUTER_DATASET_PATH`, and `ROUTER_BACKUP_DATASET_PATH`. They remain required and have no project-specific pool paths. Container mount points stay stable (`/app/config.yaml`, `/app/data`, `/app/backups`); the config file controls the database file (`database.path`), backup directory (`backup.directory`), and optional spool directory (`gateway.spool_dir`). Defaults place the database and default spool on the data mount and backups on their separate mount. Document TrueNAS ACL ownership for UID/GID 65532 and do not add host-specific paths to the repository.

### 3. First-run administrator setup

When the persistent database contains no administrator, the process generates a 256-bit random one-time setup code in unpadded base64url form, keeps it in memory, and emits it once at startup in a warning-level log entry. The browser app requests `GET /api/v1/auth/setup-state`, which returns only `{"setup_required":true|false}`, and presents the setup form instead of login while setup is pending. The form asks for the code, username, display name, password, and password confirmation; it submits those fields to `POST /api/v1/auth/setup`. The API validates the exact configured `server.public_url` Origin, request size, username/password inputs, matching confirmation, and the existing minimum password length of 12 characters. Passwords are Argon2id-hashed using the existing helper. Compare the code in constant time, rate-limit code attempts, and serialize setup so only one concurrent request can claim the first administrator.

The code is never written to YAML or SQLite, is invalidated on successful account creation, and is regenerated after a process restart if setup is still incomplete. The status endpoint reveals only whether setup is pending, not account details. The setup endpoint and UI become unavailable as soon as an administrator exists; existing users continue to the normal login flow without a setup screen. The setup response returns the user to login rather than creating a session as a side effect.

The exact HTTPS `server.public_url` remains required for browser mutations and secure session cookies. Operators must keep the service on loopback or behind an appropriately protected TLS reverse proxy while setup is pending, and treat startup logs as sensitive until the first administrator is created. No password or admin username/display name is placed in YAML. Existing databases with an administrator are unaffected; deployments that have not yet bootstrapped use the new setup flow.

Alternatives considered: keeping the existing environment-secret bootstrap avoids an unauthenticated setup endpoint but requires advance credential configuration; asking for credentials with no setup code would let the first network visitor seize administrator access. A generated one-time log code plus exact-Origin enforcement is recommended for the requested browser-first experience.

## User-visible setup and failure behavior

- A database with an administrator shows the ordinary login page; setup cannot be reopened through an API call or by replaying a prior code.
- A database without an administrator shows the setup form. Missing, stale-after-restart, consumed, or incorrect codes do not create an account and return a generic failure. Wrong Origin returns 403, a bad code returns a generic 401, rate limiting returns 429, and setup already completed returns 409.
- Wrong-Origin requests are rejected. Rate-limited attempts receive an explicit retry response without revealing whether an account was created.
- Passwords are entered only over the configured HTTPS origin, are not echoed, and are never logged or persisted in plaintext.
- A database/startup error fails closed: the setup state is not reported as available and no account is created.

## Compatibility and migration

No database migration is expected if the setup code remains process-local and no setup state is persisted. Existing database users and sessions keep their current behavior. Remove `auth.admin.bootstrap_username`, `auth.admin.display_name`, and `auth.admin.password_env` from shipped config examples and stop using them in startup. Existing YAML keys become unused and should be called out in upgrade notes; they must not silently trigger legacy password bootstrap. Keep `server.public_url` and the existing secure session/Origin protections.

## Verification and acceptance

1. Config tests cover valid/default/custom container ports and reject empty, non-numeric, zero, or out-of-range values. Confirm the listener, port mapping, and healthcheck all use the same chosen internal port.
2. Setup tests cover code generation and restart rotation, valid and invalid/consumed codes, wrong Origin, oversized input, weak password, duplicate/concurrent setup, existing-admin lockout, and successful Argon2id account creation without persisting or logging the password.
3. UI build and browser checks cover first-use, submitted-invalid, success-to-login, existing-admin login, narrow viewport, keyboard access, and accessible labels/errors.
4. GitHub Actions checks validate tag/release handling, minimal workflow permissions, SHA-pinned actions, image tags/labels, and public anonymous pull after package visibility is set.
5. Compose rendering with custom host and container ports and custom host paths must succeed. A local container smoke verifies `/health`, `/ready`, and a healthy image state with a non-default container port.
6. Run `go test ./...`, `go build ./cmd/router`, `cd frontend && npm run build`, and build/pull the release image. Document that only the on-device test verifies TrueNAS-specific ACLs, networking, backup/restore, and `flock` behavior.

## Out of scope

- Changing llama-swap ownership or enabling runtime controls.
- Enabling the inference gateway by default.
- Publishing images for platforms other than `linux/amd64` in this milestone.
- Replacing TrueNAS volume mounts with project-specific dataset names or pool paths.
- Treating container health as proof that backups, storage permissions, upstream inference, or NAS locking have been verified.

## Sequencing

This work combines deployment packaging/configuration with a security-sensitive first-admin flow and should be tracked as one proposed deployment-readiness milestone with independent implementation/review gates for those areas. The current milestone index lists the Runtime Management Extension (RME) as the next proposed milestone and says it is not approved for implementation. Before implementation begins, the user must approve this design and explicitly confirm whether this deployment-readiness milestone should be sequenced ahead of RME. RME itself remains out of scope here.

## References

- [GitHub Actions: publishing Docker images](https://docs.github.com/en/actions/tutorials/publish-packages/publish-docker-images)
- [GitHub Packages Container registry](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
- [GitHub Packages permissions and visibility](https://docs.github.com/en/packages/learn-github-packages/about-permissions-for-github-packages)
