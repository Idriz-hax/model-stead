# M9 First-Run Administrator Setup Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use subagent-driven-development (recommended) or executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace YAML-configured administrator bootstrap with a guarded one-time browser setup that stores only the Argon2id password hash.

**Architecture:** If the database has no administrator, create a process-local 256-bit setup code and emit it once in startup logs. Expose minimal setup status/claim endpoints protected by exact Origin, rate limiting, constant-time code comparison, input limits, and a serialized first-admin transaction. Once the admin exists, setup closes and the existing login flow is unchanged.

**Tech Stack:** Go `crypto/rand`, SHA-256, Argon2id helper, SQLite, existing net/http auth routes, React/TypeScript.

---

## File map

- Modify `internal/config/config.go`, both config examples, and `cmd/router/main.go`: remove YAML bootstrap credentials and old startup bootstrap.
- Modify `internal/admin/users.go`; test in `internal/admin/users_test.go`: create the first admin transactionally and audit it.
- Create `internal/server/setup.go`; modify `internal/server/auth.go`, `internal/server/server.go`, and auth/server tests: setup-code lifecycle and protected setup endpoints.
- Modify `frontend/src/api.ts`, `frontend/src/AdminApp.tsx`, and `frontend/src/style.css`: setup-state check and accessible first-admin form.
- Update `README.md` and `docs/deployment/truenas-scale.md`: log code, HTTPS/network precautions, migration from old YAML fields.

## Tasks

### Task 1: Add transactional first-admin creation

**Files:** `internal/admin/users.go`, `internal/admin/users_test.go`.

- [ ] **Step 1: Add a failing store test.** Open a migrated database with `database.Open`, seed the existing `scheduler_settings` row as in `TestUserLifecycle`, then call the new method twice:

```go
store := Store{DB: db}
first, err := store.CreateFirstAdmin(context.Background(), User{
	Username: "owner", DisplayName: "Owner", Role: "admin", SchedulingWeight: 1,
}, "a sufficiently long password")
if err != nil || first.ID == "" || first.Role != "admin" {
	t.Fatalf("first admin=%+v err=%v", first, err)
}
if _, err := store.CreateFirstAdmin(context.Background(), User{
	Username: "second", DisplayName: "Second", Role: "admin", SchedulingWeight: 1,
}, "another sufficiently long password"); err == nil {
	t.Fatal("created a second initial administrator")
}
```

Also verify the stored value passes `auth.VerifyPassword` and is not equal to the submitted password; verify one `admin.bootstrap` audit event exists.
- [ ] **Step 2: Verify the new test fails.** Run `go test ./internal/admin -run '^TestCreateFirstAdmin$'`; expected: compile failure because `CreateFirstAdmin` is absent.
- [ ] **Step 3: Implement transactional creation.** Add this method in `internal/admin/users.go`:

```go
func (s Store) CreateFirstAdmin(ctx context.Context, u User, password string) (User, error) {
	u.Username = strings.TrimSpace(u.Username)
	if len(password) < 12 || len(password) > 1024 {
		return User{}, errors.New("password must be 12-1024 bytes")
	}
	if u.Role != "admin" {
		return User{}, errors.New("first user must be an administrator")
	}
	capacity, err := s.capacity(ctx)
	if err != nil {
		return User{}, err
	}
	if err := validateUser(u, capacity); err != nil {
		return User{}, err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, err
	}
	u.ID, u.Status, u.MustChangePassword = uuid.NewString(), "active", false
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	// Take SQLite's write lock before checking so claims from separate
	// managers/connections cannot both observe an empty admin table.
	if _, err = tx.ExecContext(ctx, `UPDATE scheduler_settings SET updated_at=updated_at WHERE id=1`); err != nil {
		return User{}, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin'`).Scan(&count); err != nil {
		return User{}, err
	}
	if count != 0 {
		return User{}, errors.New("administrator already exists")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,display_name,password_hash,role,status,scheduling_weight,max_queued,max_active_contended,must_change_password) VALUES(?,?,?,?,?,'active',?,?,?,0)`, u.ID, u.Username, u.DisplayName, hash, u.Role, u.SchedulingWeight, u.MaxQueued, u.MaxActiveContended)
	if err != nil {
		return User{}, err
	}
	if err = insertAudit(ctx, tx, u.ID, "admin.bootstrap", "user", u.ID, Redacted{}, userRedacted(u)); err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}
```

- [ ] **Step 4: Verify the store test passes.** Run `go test ./internal/admin -run '^TestCreateFirstAdmin$'`; expected: PASS.
- [ ] **Step 5: Commit the store behavior.** Commit with `feat: add transactional first-admin creation`.

### Task 2: Add the one-time setup code and API

**Files:** Create `internal/server/setup.go`; modify `internal/server/server.go`, `internal/server/auth.go`, and `internal/server/auth_test.go`.

- [ ] **Step 1: Add a failing HTTP setup test.** Create a fresh migrated database, create a setup manager, and assert that `GET /api/v1/auth/setup-state` returns `{"setup_required":true}`. Submit a valid JSON setup body with an incorrect Origin and assert 403; submit the same body with the exact Origin and generated code and assert 201; repeat the request and assert 409. Verify the stored password is a hash and the admin count is exactly one.
- [ ] **Step 2: Verify the route test fails.** Run `go test ./internal/server -run '^TestFirstAdminSetup'`; expected: setup status route is 404.
- [ ] **Step 3: Implement setup-code lifecycle in `setup.go`.** Add `SetupManager` with `db *sql.DB`, a mutex, active flag, and `[32]byte` SHA-256 code hash. `NewSetupManager(ctx, db)` counts administrators; if zero, generate 32 bytes with `crypto/rand`, encode with `base64.RawURLEncoding`, store only `sha256.Sum256([]byte(code))`, and return the plaintext code to startup for one log line. If an administrator exists, return an inactive manager and empty code. `Required()` reads the active flag under the mutex. `Claim(ctx, code, user, password)` serializes attempts, rejects inactive/incorrect codes with sentinel errors and `subtle.ConstantTimeCompare`, calls `admin.Store{DB: s.db}.CreateFirstAdmin`, and clears the hash/active flag only after success.
- [ ] **Step 4: Register setup endpoints.** Add `Setup *SetupManager` to `server.AuthConfig` and register `GET /api/v1/auth/setup-state` plus `POST /api/v1/auth/setup` in `registerAuth`. Status returns only `{"setup_required":bool}`. POST requires the exact existing Origin check, calls `allowLogin(r.RemoteAddr)` before Argon2 work, limits the body to 4096 bytes, requires matching passwords and 12–1024 bytes, and uses the existing username/display-name validators through `CreateFirstAdmin`. Return 201 on success, 403 wrong Origin, 401 for a bad code, 429 when rate-limited, 409 once setup is closed, 400 for invalid fields, and 503 for storage/hash failures. Never include the code or password in response text or logs.
- [ ] **Step 5: Verify security and concurrency tests.** Add cases for invalid code, oversized body, short password, setup code rotation after constructing a new manager, concurrent claims through separate `SetupManager`/DB connections (exactly one succeeds), an admin created after manager initialization (claim fails and the stale manager closes setup), and existing-admin state (`setup_required:false`, setup POST 409). Run `go test ./internal/server -run '^TestFirstAdminSetup'`; expected: PASS.
- [ ] **Step 6: Commit the setup API.** Commit `internal/server/setup.go`, server/auth files, and tests with `feat: add guarded first-admin setup API`.

### Task 3: Replace configured bootstrap with browser onboarding

**Files:** `internal/config/config.go`, `cmd/router/main.go`, both config examples, `deploy/truenas/compose.yaml`, `frontend/src/api.ts`, `frontend/src/AdminApp.tsx`, and `frontend/src/style.css`.

- [ ] **Step 1: Remove bootstrap credentials from configuration.** Delete the `Auth.Admin` fields and the validation block for `auth.admin.*` from `config.go`. Remove `auth.admin` from `config.example.yaml` and `deploy/truenas/config.example.yaml`; remove `ADMIN_BOOTSTRAP_PASSWORD` from TrueNAS Compose. Existing YAML keys are ignored and must never create an account.
- [ ] **Step 2: Remove startup password bootstrap.** Delete the `bootstrapAdmin` call/function and its `uuid` import from `cmd/router/main.go`. After database migrations and scheduler settings are ready, call `server.NewSetupManager(ctx, db)`. If setup is pending, fail startup unless `server.public_url` is a configured HTTPS origin; otherwise log the returned one-time code at warning level exactly once and pass the manager into `server.AuthConfig`. Existing databases with an admin must start without a setup code.
- [ ] **Step 3: Add API client methods.** In `frontend/src/api.ts`, add `setupState()` for `GET /api/v1/auth/setup-state` and `setupAdmin(code, username, display_name, password, password_confirmation)` for `POST /api/v1/auth/setup`, using the existing same-origin `request` helper.
- [ ] **Step 4: Integrate setup state into the app gate.** In `AdminApp.tsx`, after `api.me()` fails, request `setupState()` before rendering login. Render the setup form only when `setup_required` is true; if setup-state loading fails, show a retryable unavailable message instead of falling through to login.
- [ ] **Step 5: Build the accessible setup form.** Have `@designer` implement the form in `AdminApp.tsx` and `style.css` using existing `.auth` styles. Include labeled code/username/display-name/password/confirmation inputs, `autocomplete="username"` / `new-password`, minimum length feedback, mismatched-password error, busy/disabled submit, and `role="alert"` error announcements. On success, clear all fields and show the ordinary login form; do not persist the code or password in browser storage.
- [ ] **Step 6: Verify the frontend.** Run `cd frontend && npm run build`; expected: TypeScript and Vite pass.
- [ ] **Step 7: Commit the onboarding UI and config removal.** Commit with `feat: replace YAML admin bootstrap with first-run setup`.

### Task 4: Document, security-review, and verify

**Files:** `README.md`, `docs/deployment/truenas-scale.md`, `docs/milestones.md`, plus all M9 files above.

- [ ] **Step 1: Update setup instructions.** Remove all instructions and environment mappings for bootstrap username/password. Explain first startup: restrict access to loopback/TLS, set exact HTTPS `server.public_url`, read the one-time code in app logs, create the admin in browser, then sign in. Explain code rotation after restart and that logs remain sensitive until setup completes.
- [ ] **Step 2: Test startup compatibility.** Add an integration test proving an existing database with an admin starts with setup disabled and the old YAML bootstrap fields cannot create or alter an account. Verify a no-admin database with missing `server.public_url` fails closed without logging a setup code.
- [ ] **Step 3: Run required checks.** Run `go test ./...`, `go build ./cmd/router`, `make frontend`, and `(cd frontend && npm run build)`; expected: all pass.
- [ ] **Step 4: Browser-smoke setup states.** With a fresh test database, verify setup form, wrong-code error, password mismatch/short-password feedback, success to login, and no setup form after the admin exists. Check desktop/mobile width, labels, focus, and keyboard-only submission using Playwright when browser tooling is available; record this as a deferred gate if unavailable.
- [ ] **Step 5: Obtain independent security review.** Run `@security-reviewer` on setup API, setup-code logging/lifecycle, first-admin transaction, and the release workflow before shipping. Resolve all material findings and repeat focused tests.
- [ ] **Step 6: Update milestone status after both M9 tracks pass review.** Record M9 complete in `docs/milestones.md`; keep M7 hardware validation outstanding until the operator verifies the actual NAS.
- [ ] **Step 7: Commit verified docs and milestone record.** Commit with `docs: document first-run administrator setup`.
