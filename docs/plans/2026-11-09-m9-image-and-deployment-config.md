# M9 Image Publishing and Deployment Configuration Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish versioned public GHCR images and allow TrueNAS operators to set both ports and all persistent host paths without code/config mismatch.

**Architecture:** A least-privilege release workflow builds `linux/amd64` and pushes version plus `latest` tags to GHCR. TrueNAS Compose provides independent host and container port values; the same container port drives the Go listener, healthcheck, and Compose target. Existing host-path variables and app-level database/backup/spool config remain the storage controls.

**Tech Stack:** Go, YAML, Docker Buildx, GitHub Actions, GHCR, TrueNAS SCALE Compose.

---

## File map

- Create `.github/workflows/publish-image.yml`: release-only GHCR publishing.
- Modify `internal/config/config.go` and `internal/config/config_test.go`: validate and apply `ROUTER_CONTAINER_PORT` to the configured listener address.
- Modify `cmd/router/main.go` and `cmd/router/main_test.go`: make the image healthcheck probe that same port.
- Modify `Dockerfile`, `deploy/truenas/compose.yaml`, both config examples, `README.md`, and `docs/deployment/truenas-scale.md`: public image, both port inputs, storage-path instructions.
- Modify `docs/milestones.md` only after implementation passes review: record M9 completion while retaining the M7 hardware-validation caveat.

## Tasks

### Task 1: Validate and wire the internal port (TDD)

**Files:** `internal/config/config.go`, `internal/config/config_test.go`, `cmd/router/main.go`, `cmd/router/main_test.go`.

- [ ] **Step 1: Test configured and invalid internal ports.** In `config_test.go`, add a test using the existing inline YAML setup:

```go
func TestLoadAppliesContainerPortOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	db := filepath.Join(t.TempDir(), "router.db")
	if err := os.WriteFile(path, []byte("server:\n  address: '0.0.0.0:8080'\ndatabase:\n  path: "+db+"\nlogging:\n  level: info\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROUTER_CONTAINER_PORT", "9091")
	c, err := Load(path)
	if err != nil || c.Server.Address != "0.0.0.0:9091" {
		t.Fatalf("address=%q err=%v", c.Server.Address, err)
	}
}
```

- [ ] **Step 2: Verify the new config test fails.** Run `go test ./internal/config -run '^TestLoadAppliesContainerPortOverride$'`; expected: listener address remains `0.0.0.0:8080`.
- [ ] **Step 3: Implement validated port override.** Add `ContainerPort() (string, error)` to `config.go`: return `8080` when `ROUTER_CONTAINER_PORT` is unset; otherwise parse a base-10 integer and reject empty, non-numeric, zero, or values above 65535. In `Load`, only apply the override when the variable is present; use `net.SplitHostPort(c.Server.Address)` and `net.JoinHostPort(host, port)` so the bind host remains unchanged. Add table tests for `""`, `"abc"`, `"0"`, and `"65536"`; each must fail config loading.
- [ ] **Step 4: Test the healthcheck target.** Extract a small `healthcheckURL() (string, error)` in `cmd/router/main.go`; it uses `config.ContainerPort()` and `net.JoinHostPort("127.0.0.1", port)` to produce `http://127.0.0.1:<port>/ready`. Test it with `ROUTER_CONTAINER_PORT=9091` and expect `http://127.0.0.1:9091/ready`.
- [ ] **Step 5: Make healthcheck use the helper.** Keep the existing HTTP status check, but request the helper URL and exit nonzero if URL derivation or the request fails.
- [ ] **Step 6: Run focused Go tests.** Run `go test ./internal/config ./cmd/router`; expected: PASS.
- [ ] **Step 7: Commit the port behavior.** Commit `internal/config/` and `cmd/router/` with message `feat: configure container listener port`.

### Task 2: Configure the TrueNAS Compose deployment

**Files:** `Dockerfile`, `deploy/truenas/compose.yaml`, `deploy/truenas/config.example.yaml`, `README.md`, `docs/deployment/truenas-scale.md`.

- [ ] **Step 1: Set the image defaults and variables in Compose.** Keep the stable container mount points and required host paths. Update the service to:

```yaml
    image: ${ROUTER_IMAGE:-ghcr.io/idriz-hax/model-stead:latest}
    environment:
      CONFIG_FILE: /app/config.yaml
      ROUTER_CONTAINER_PORT: ${ROUTER_CONTAINER_PORT:-8080}
      LLAMA_SWAP_API_KEY: ${LLAMA_SWAP_API_KEY:-}
    ports:
      - "${ROUTER_BIND_ADDRESS:-127.0.0.1}:${ROUTER_HOST_PORT:?Set ROUTER_HOST_PORT}:${ROUTER_CONTAINER_PORT:-8080}"
    volumes:
      - ${ROUTER_CONFIG_PATH:?Set ROUTER_CONFIG_PATH}:/app/config.yaml:ro
      - ${ROUTER_DATASET_PATH:?Set ROUTER_DATASET_PATH}:/app/data
      - ${ROUTER_BACKUP_DATASET_PATH:?Set ROUTER_BACKUP_DATASET_PATH}:/app/backups
```

- [ ] **Step 2: Match the image default.** In `Dockerfile`, keep `EXPOSE 8080` as default metadata and add `ENV ROUTER_CONTAINER_PORT=8080`; do not imply `EXPOSE` allocates a host port.
- [ ] **Step 3: Document operator-controlled locations.** Keep required config/data/backup host paths in Compose. In the TrueNAS config example and guide, explain that `database.path` must remain inside `/app/data`, `backup.directory` inside `/app/backups`, and `gateway.spool_dir` inside `/app/data` unless the operator adds a separate volume. Keep `/app/config.yaml`, `/app/data`, and `/app/backups` as stable container targets.
- [ ] **Step 4: Document port selection and pulls.** Explain that `ROUTER_HOST_PORT` must be a free host TCP port, `ROUTER_CONTAINER_PORT` defaults to 8080 and must match no other field because Compose/app/healthcheck share it, and only the host-side port conflicts with other apps. Document setting `ROUTER_IMAGE` to a specific release tag for pinning.
- [ ] **Step 5: Render Compose with non-default inputs.** Run:

```sh
ROUTER_HOST_PORT=18081 ROUTER_CONTAINER_PORT=19081 \
ROUTER_CONFIG_PATH=/mnt/tank/apps/model-stead/config.yaml \
ROUTER_DATASET_PATH=/mnt/tank/apps/model-stead/data \
ROUTER_BACKUP_DATASET_PATH=/mnt/tank/backups/model-stead \
docker compose -f deploy/truenas/compose.yaml config --quiet
```

Expected: exit 0; inspect `docker compose ... config` and confirm the published target is 19081 and each source path is preserved.
- [ ] **Step 6: Commit deployment configuration.** Commit these files with `feat: configure TrueNAS image ports and storage paths`.

### Task 3: Publish release images to GHCR

**Files:** Create `.github/workflows/publish-image.yml`; modify `README.md` and `docs/deployment/truenas-scale.md`.

- [ ] **Step 1: Add the release workflow.** Use this complete workflow; `actions/checkout` is pinned to verified v4.2.2 commit `11bd71901bbe5b1630ceea73d27597364c9af683`:

```yaml
name: Publish container image
on:
  release:
    types: [published]
permissions: {}
jobs:
  publish:
    if: github.event.release.prerelease == false
    runs-on: ubuntu-24.04
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@11bd71901bbe5b1630ceea73d27597364c9af683
        with:
          ref: ${{ github.event.release.tag_name }}
          persist-credentials: false
      - name: Validate version tag
        env:
          RELEASE_TAG: ${{ github.event.release.tag_name }}
        run: |
          if [[ ! "$RELEASE_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            echo "Expected a stable vMAJOR.MINOR.PATCH release tag" >&2
            exit 1
          fi
      - name: Log in to GHCR
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: echo "$GH_TOKEN" | docker login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
      - name: Build and publish
        env:
          RELEASE_TAG: ${{ github.event.release.tag_name }}
        run: |
          image="ghcr.io/${GITHUB_REPOSITORY,,}"
          docker buildx build --platform linux/amd64 --push \
            --tag "$image:$RELEASE_TAG" \
            --tag "$image:latest" \
            --label "org.opencontainers.image.source=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY" \
            --label "org.opencontainers.image.revision=$GITHUB_SHA" .
```

- [ ] **Step 2: Inspect workflow security and syntax.** Confirm tag checkout is release-bound, prereleases are skipped, permissions are only `contents: read` and `packages: write`, login uses `GITHUB_TOKEN`, and no pull-request event can publish. Run `actionlint .github/workflows/publish-image.yml` if installed; otherwise inspect the workflow and let GitHub Actions validate on the first release.
- [ ] **Step 3: Document image pulls.** Document `docker pull ghcr.io/idriz-hax/model-stead:<version>` and the `latest` tag. Explain that the first package must be changed to public once in GitHub package settings; do not add a registry password to Compose.
- [ ] **Step 4: Commit the release workflow.** Commit with `ci: publish versioned images to GHCR`.

### Task 4: Verify the deployment package

**Files:** All changed files in Tasks 1–3.

- [ ] **Step 1: Run the required application checks.** Run `make frontend && go test ./... && go build ./cmd/router && (cd frontend && npm run build)`; expected: all Go packages pass, binary builds, and Vite completes.
- [ ] **Step 2: Build and smoke a non-default-port container.** When Docker Buildx is available, run `docker buildx build --platform linux/amd64 --load -t model-stead:m9-smoke .`, then start it with `docker run --rm -d --name model-stead-m9 -e ROUTER_CONTAINER_PORT=19081 -p 127.0.0.1:18081:19081 model-stead:m9-smoke`. Verify `curl -fsS http://127.0.0.1:18081/health` and `/ready` both return `{"status":"ok"}`, and Docker reports healthy. Stop the smoke container after checking. If Docker is unavailable, record this as a deferred container acceptance gate.
- [ ] **Step 3: Verify GHCR behavior after an approved version is chosen.** Do not invent a release version. This remains a deferred external acceptance gate until the owner selects a stable tag and publishes its GitHub Release. Then verify Actions publishes both the versioned and `latest` image; set package visibility to public, then run `docker logout ghcr.io && docker pull ghcr.io/idriz-hax/model-stead:<selected-version>` from an unauthenticated session.
- [ ] **Step 4: Close the deployment milestone after review.** Update `docs/milestones.md` to record M9 complete only after its implementation/security review passes; retain the M7 TrueNAS hardware-validation caveat until the operator validates the actual NAS.
- [ ] **Step 5: Commit final documentation and checks.** Commit any remaining guide/index edits with `docs: document GHCR and configurable TrueNAS deployment`.
