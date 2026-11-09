.PHONY: test build frontend
frontend:
	cd frontend && npm ci && npm run build && rm -rf ../web/dist && mkdir -p ../web/dist && cp -R dist/. ../web/dist/
test:
	go test ./...
build: frontend
	mkdir -p bin
	go build -o bin/router ./cmd/router
