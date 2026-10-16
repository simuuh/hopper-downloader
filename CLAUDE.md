# Hopper — notes for Claude Code

Privacy-first, self-hosted download manager. Go 1.26 daemon (`hopperd`),
client CLI (`hopperctl`), React Web UI embedded via `go:embed`, MV3
browser extension.

**Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) first** — it explains
the queue, download engine, plugin priority rules, Click'n'Load/DLC
protocols and deployment quirks. Plugin authoring:
[docs/PLUGIN_GUIDE.md](docs/PLUGIN_GUIDE.md). Brand/UI rules:
[docs/BRAND.md](docs/BRAND.md).

## Commands

```sh
make web                         # npm ci + build web/dist (needed before go build)
go build ./... && go test ./...  # tests need web/dist (or an empty web/dist/index.html)
golangci-lint run ./...          # v2 config, must report 0 issues
cd web && npm run lint && npm run build
node extension/test/validate-extension.js
```

## Rules

- `internal/queue.Manager` is the only writer of queue state; publish
  copies, never live pointers.
- Hoster plugins are compiled in; optional capabilities are separate
  interfaces checked by type assertion.
- `log/slog` injected explicitly, no package-level mutable state,
  errors wrapped with context.
- Every filesystem path derived from a hoster or user name goes through
  `internal/pathsafe`.
- No new Go dependencies unless strictly necessary.
- Comments describe behavior and reasons, not task history.
- Test data uses neutral placeholders — never real container IDs,
  release names or third-party file links.
- Hoster API facts: mark what was verified against the live API and
  what wasn't (see the table in ARCHITECTURE.md).
- Web UI: colors only via the CSS custom properties in `styles.css`
  (both themes), dense layout per BRAND.md.
- Git: start every branch from `origin/main` after `git fetch` — local
  `main` is often stale because PRs are merged on GitHub and work happens
  in parallel sessions; merge `origin/main` again before pushing/opening a
  PR. See CONTRIBUTING.md, "Common pitfalls".
- Commits: one topic per commit, always — split a file touched by several
  topics with `git add -p` or a staged intermediate version, never with
  `git apply --unidiff-zero`.
