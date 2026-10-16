.PHONY: web build test lint check docker clean

# The web UI must be built before `go build`/`go test`: cmd/hopperd and
# the web package embed web/dist via go:embed, and that directory is not
# committed (see .gitignore) — CI and local dev both start here.
web:
	cd web && npm ci && npm run build

build: web
	go build ./...

test: web
	go vet ./...
	go test ./...

lint: web
	go vet ./...
	cd web && npm run lint

# check runs everything CI runs.
check: web
	go vet ./...
	go test ./...
	golangci-lint run ./...
	cd web && npm run lint
	for f in extension/*.js; do node --check "$$f" || exit 1; done
	node extension/test/validate-extension.js

docker:
	docker build -f docker/Dockerfile -t hopper:local .

clean:
	rm -rf web/dist web/node_modules
