# Contributing to Hopper

Thanks for helping! Bug reports, plugin contributions and fixes are all
welcome. Please read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) before
larger changes.

## Prerequisites

- Go 1.26+
- Node 24+ (Web UI, extension checks)
- [golangci-lint](https://golangci-lint.run/) v2
- Docker (optional, for the container build)

## Building and checking

```sh
make web     # npm ci + build web/dist (embedded into hopperd via go:embed)
make build   # go build ./...
make test    # go vet + go test ./...
make check   # everything CI runs: tests, golangci-lint, web lint/tests/build, extension checks
make docker  # docker build -f docker/Dockerfile -t hopper:local .
```

`web/dist` isn't committed; build it once before `go build`/`go test`.

Run it locally:

```sh
cp hopper.example.yaml hopper.yaml
export HOPPER_JWT_SECRET=$(openssl rand -base64 32)
export HOPPER_ENCRYPTION_KEY=$(openssl rand -base64 24 | cut -c1-32)
export HOPPER_ADMIN_PASSWORD_HASH=$(printf '%s' 'dev' | go run ./cmd/hopperctl hash-password)
go run ./cmd/hopperd -config hopper.yaml   # → http://localhost:9667
```

For Web UI work, `cd web && npm run dev` serves the UI with hot reload
and proxies the API to a running hopperd.

## Pull requests

- `make check` passes, `gofmt -l .` is empty.
- New behavior comes with a test. Prefer real dependencies over mocks
  where it's cheap: `httptest.Server` for HTTP, a temp-dir bbolt
  database for persistence.
- Test data uses neutral placeholders (`example.part1.rar`,
  `https://host.example/...`) — never real container IDs, release names
  or links to third-party content.
- Keep commits focused: one logical change per commit — also when one
  file carries changes for several topics (see the pitfall below).
- Branch from the *remote* main and bring it in again before opening or
  updating a PR:
  ```sh
  git fetch origin
  git switch -c feature/xyz origin/main      # not from a local main
  # ... work, commit ...
  git fetch origin && git merge origin/main  # before push / PR
  ```
- `main` is meant to be protected by a ruleset requiring these CI checks:
  `Detect code changes`, `Go (vet, test -race, lint)` and
  `Web UI (lint + build)`. The workflow always runs; for a PR that only
  touches docs (`*.md`, `docs/`, `site/`) the Go and Web jobs are
  skipped, which counts as passed — so don't add a paths filter to
  `ci.yml`, or docs-only PRs would wait forever for checks that never
  report.

## Code style

- Every exported identifier has a doc comment. Comments explain behavior
  and *why*, not how the code came to be.
- Interfaces at package boundaries that need testing in isolation (see
  `api.QueueService`, `queue.AccountLookup`), not for single
  implementations without a testing need.
- `log/slog`, injected explicitly; no package-level mutable state.
- Errors wrapped with context: `fmt.Errorf("...: %w", err)`.
- Web UI colors only via the CSS custom properties (dark and light
  theme), see [docs/BRAND.md](docs/BRAND.md).

## Adding a hoster plugin

See [docs/PLUGIN_GUIDE.md](docs/PLUGIN_GUIDE.md). Note in the plugin's
doc comment which API facts were verified against the live service and
which weren't.

## Security-sensitive changes

Changes to `internal/config` validation, `internal/pathsafe`,
`internal/account` encryption, JWT handling in `api/`, or the
Click'n'Load trusted-CIDR gate need their reasoning explained in the PR
description.

## Reporting a security issue

Please don't open a public issue — see [SECURITY.md](SECURITY.md).

## Common pitfalls

- A binary built at the repo root (`/hopperd`, `/hopperctl`) is ignored
  by `.gitignore`; `git check-ignore -v <path>` shows why a file isn't
  picked up.
- **"This branch has conflicts that must be resolved" on a fresh PR.**
  Usually the branch was created from a stale *local* `main`: PRs merged
  on GitHub (or from another clone/session) don't update it. A branch
  started that way misses everything merged since and collides with
  sweeping changes on main — a module-path rename touches every import,
  a README/docs restructuring or a Web UI redesign touches the files
  feature work also edits. Fix: `git fetch origin && git merge
  origin/main`, resolve, re-run `make check`. Avoid: branch from
  `origin/main` (above), and keep README/docs restructurings in their
  own PR instead of bundling them with a feature.
- **Splitting one file across commits:** use `git add -p` (or stage a
  hand-built intermediate version of the file). Don't filter `git diff
  -U0` hunks into `git apply --cached --unidiff-zero` — without context
  lines, pure insertions can land at the wrong place in the index while
  `git apply` reports success.
- `docker compose up` failing with "permission denied": see
  [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md#downloads-permission-denied)
  (usually `PUID`/`PGID` not matching an existing downloads folder).
