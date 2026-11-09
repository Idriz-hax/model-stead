# Agent guidance

- The approved product specification authorizes staged implementation through its milestones; implement only the current milestone and keep later features out of scope until that milestone passes review.
- Run `go test ./...`, `go build ./cmd/router`, and `cd frontend && npm run build` for relevant changes.
- Database changes belong in forward-only Goose migrations under `internal/database/migrations`.
- Keep the browser app static and embed its production build in the Go binary.
- The API gateway is disabled by default; do not expose it before client authentication exists.
