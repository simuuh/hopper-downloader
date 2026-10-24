# Architecture

How Hopper is built and why. For using it see the [README](../README.md);
for writing a hoster plugin see [PLUGIN_GUIDE.md](PLUGIN_GUIDE.md).

- [Components](#components)
- [Package layout](#package-layout)
- [Life of a link](#life-of-a-link)
- [Queue](#queue)
- [Download engine](#download-engine)
- [Plugins](#plugins)
- [Archive extraction](#archive-extraction)
- [Click'n'Load](#clicknload)
- [DLC containers](#dlc-containers)
- [Accounts](#accounts)
- [API, auth and Web UI](#api-auth-and-web-ui)
- [hopperctl](#hopperctl)
- [Browser extension](#browser-extension)
- [Deployment](#deployment)
- [Security model](#security-model)
- [Known gaps](#known-gaps)

## Components

| Binary / part | Runs on | Role |
| --- | --- | --- |
| `hopperd` | NAS / server | Queue, download engine, REST API, WebSocket, embedded Web UI, Click'n'Load listener |
| `hopperctl` | client PC (optional) | CLI client, local Click'n'Load relay, `.dlc` folder watcher, OS service |
| `extension/` | browser (optional) | Alternative to `hopperctl` for the remote setup: intercepts Click'n'Load and `.dlc` downloads |

Go 1.26+ (`toolchain go1.26.9` — a supported release; golangci-lint and
govulncheck must be able to load it), no CGO. All state lives in one
bbolt file (`database.path`), stamped with its layout version
(`internal/dbschema`): hopperd refuses a database a newer release wrote
instead of misreading it after a downgrade. A layout change that an
older release would misread bumps the version and adds its migration
there.

## Package layout

```
cmd/hopperd        daemon wiring (main.go is the only place that knows every package)
cmd/hopperctl      client CLI, CnL proxy, folder watch, service install
api/               REST /api/v1, JWT auth, WebSocket /ws, OpenAPI spec
internal/queue     packages/items, dispatcher, grouping (AddLinks), extraction trigger
internal/downloader chunked resumable HTTP engine, bandwidth limits
internal/extract   zip/tar via stdlib, rar via external unrar
internal/clicknload Click'n'Load 1/2 listener
internal/dlc       DLC container decryption
internal/account   encrypted hoster credentials, validation cache
internal/settings  runtime-editable settings (bbolt) with live hooks
internal/stats     daily/all-time download statistics
internal/notify    "package finished" push: JSON webhook and/or ntfy
internal/update    opt-in check for a newer release (GitHub API)
internal/config    hopper.yaml + HOPPER_* env overrides
internal/dbschema  layout version of the bbolt database
internal/pathsafe  path-traversal-safe joins and filename sanitizing
internal/scheduler timers for the dispatcher
plugins/           Hoster interface + one package per hoster
web/               React UI, built to web/dist and embedded via go:embed
extension/         MV3 browser extension (Chrome/Edge/Firefox 128+)
```

Conventions: `log/slog` injected explicitly (no global logger), no
package-level mutable state, interfaces at package boundaries that need
testing in isolation, errors wrapped with context, `golangci-lint` with 0
issues.

## Life of a link

1. A link arrives — pasted (`POST /api/v1/links/bulk`), imported
   (`POST /api/v1/import`, `.dlc`/`.txt`), via Click'n'Load, or as a
   single `POST /api/v1/links`.
2. `queue.Manager.AddLinks` expands container links, drops duplicates,
   groups archive parts per release and merges them into an existing
   package of the same release (see [Queue](#queue)).
3. The dispatcher admits items up to `parallel_files`, top to bottom in
   the order the Web UI shows (`queueOrder`, see [Queue](#queue)).
4. `run()` re-resolves which plugin handles the URL (accounts may have
   changed since the item was queued), calls `Resolve` and
   `FetchDownloadURL`, and hands the direct URL to the download engine.
5. On completion the item's archive group is checked; once every part
   is done and on disk, extraction starts.
6. Every state change is published as an event and streamed to the Web
   UI over `/ws`.

## Queue

`internal/queue.Manager` is the single writer of queue state (items,
packages). Everything else reads snapshots or calls its methods.

**States.** `queued → downloading → done | error`, plus `paused`.
Waiting conditions are fields, not states: `scheduled_at`,
`retry_after` (hoster wait or automatic retry) and `captcha_url` keep an
item `queued` but not runnable. This keeps every `State` switch in the
codebase small; the UI derives a "waiting"/"captcha" badge from the
fields. Extraction status is likewise a separate `extract_state`.

**Admission.** `parallel_files` is an atomic counter with a CAS loop
(`tryAdmit`), not a channel semaphore, so it can change live. `dispatch`
runs from two goroutines (poll tick and wake channel); a plain
load-then-increment would over-admit. `Shutdown` sets a flag under
`closingMu` so no run is admitted while it waits for in-flight ones.

**Runs.** Each run of an item has its own handle (context, cancel, and
why it was stopped: pause, cancel or shutdown), registered by
`pickNext` under the lock that marks the item downloading and removed
only by that run. A plugin call that ignores its context can keep a run
alive after a pause; until it returns, `pickNext` doesn't start the item
again, and the old run can't touch a later run's handle. A run that ends
while the Manager is stopping counts as shut down — hopperd's signal
context is cancelled before `Shutdown` records the intent — so it goes
back to `queued` without using an automatic retry. A panic in a run
fails that item ("internal error") instead of ending hopperd.

**Damaged records.** A stored item or package that no longer decodes is
moved to the `queue_quarantine` bucket (key `<bucket>/<id>`) and logged;
the rest of the queue loads.

**Grouping and merging (`AddLinks`, `grouping.go`).**
- Container links (`plugins.Expander`, e.g. hide.cx) are expanded first.
- Archive parts are grouped by the lowercased `extract.GroupBase` of the
  filename (caller-supplied, else derived from the URL — hoster URLs like
  `…/movie.part1.rar.html` carry the real name). That name is stored on
  the item immediately so ordering and extraction work before the
  download starts.
- An existing package is matched by its *items'* group key, not its
  name — a DLC package called "Movie (2026)" and a later Click'n'Load of
  `movie.part4.rar` end up together, which extraction requires.
- Duplicates — same URL already queued, or same filename already in the
  target package (a mirror) — are skipped and reported. An
  all-duplicates submission is not an error.
- Naming: the caller's name for a single-release submission, otherwise
  one package per release named after its GroupBase. Non-archive links
  go into one package; a single plain link without a name stays
  package-less.
- `addMu` serializes the decision so two concurrent submissions of the
  same release can't both create a package.

**Start order (`order.go`).** One definition, used by the dispatcher and
`List` (and mirrored in `web/src/lib/order.ts`), so top to bottom in the
UI is the start order: packages by their highest unfinished priority,
then by where they were first queued (smallest `Position` — parts merged
later keep their package's place); within a package by item priority,
archive part number, `Position`. Free slots therefore go to the topmost
package first. Resume/retry of several items (`ResumeAll`, package and
multi-select actions, `POST /queue/actions`) requeue them atomically with
one dispatcher wake — requeueing one by one let whichever came first out
of a map start first.

**Sequential parts (`queue.sequential_parts`, default on).** Parts never
overtake each other: a part waits while a lower-numbered part of its set
is still `queued` (e.g. on a hoster's `retry_after`). They may run side
by side. Errored or paused parts don't block, so a set can't get stuck.

**Priority** is `low=-1`, `normal=0`, `high=1`: the zero value is
"normal", so add paths without an explicit priority don't end up "low"
(only the names are persisted).

**Automatic retry (`queue.auto_retry`, default 3).** Transient failures
(download errors, resolve/fetch errors other than "not found") re-queue
the item with an exponential `retry_after` (from `auto_retry_delay`,
max 30 min). Each retry fetches a fresh download link and resumes from
the bytes on disk. Stats count only the final failure; a manual
retry/resume resets the budget.

**Late binding.** The plugin for a URL is resolved again on every run
(`MatchHoster`), and `HosterKey` is corrected if it changed. Adding a
Real-Debrid account therefore applies to already-queued items. When a
plugin fails with `ErrAccountRequired`, the next candidate plugin is
tried; if none is left, the plugin's `FreeDownloader` fallback runs.

**Packages** are removed when their last item goes; orphans are dropped
on startup. Package actions (pause/resume/retry/delete/rename) reuse the
single-item methods.

**Editing (`edit.go`).** `EditItem` changes URL and/or filename of a
queued, paused or failed item (`PATCH /queue/{id}`); a new URL re-matches
the plugin and gives a failed item a fresh start. `AddToPackage`
(`POST /packages/{id}/links`) reuses AddLinks' expansion and duplicate
check but puts every new item into that one package.

**Package finished (`finished.go`).** `checkFinished` runs from
`complete()` (after `maybeExtract` has claimed the archive group), a
final `fail()`, and a `defer` in `runExtraction` registered *before* the
`extractTriggered` cleanup (defers are LIFO, so it runs after it). A
package is finished when every item is done or failed, none is
`extracting` and no extraction trigger for it is pending; a package-less
item counts as a package of one. The outcome last notified is remembered
per package (counts + extract result), so a retry, merged parts or a
re-extract that changes it notifies again and duplicate triggers don't.
`internal/notify` delivers in the background; `POST /notify/test` sends
synchronously.

## Download engine

`internal/downloader.Engine` downloads an already-resolved direct URL to
`BaseDir/RelPath` (always via `pathsafe.SafeJoin`).

- **Probe:** HEAD, and a ranged GET when HEAD reports no
  `Accept-Ranges` (many CDNs omit it but honor ranges).
- **Chunks:** `parallel_chunks` ranges of at least 4 MiB each, written
  with `WriteAt` into `<file>.part`; progress is persisted in
  `<file>.part.json` (throttled) so a restart resumes. Missing `.part`
  data with existing state starts over instead of zero-filling.
- **Integrity:** a chunk that ends early (clean EOF before its range is
  complete) is an error and resumes; writes are bounded to the chunk's
  range; a server answering a ranged request with `200` fails
  permanently instead of corrupting the file; a `text/html` response for
  a non-HTML target fails ("link expired?"). Only probe-confirmed sizes
  are enforced — hoster-reported sizes may be rounded.
- **Throughput:** 1 MiB reads unthrottled (measured ~2× faster than
  32 KiB); with a limit active, reads are capped to the limiter's burst
  for smooth progress.
- **Stalls:** a chunk that receives no data for 60 s (`StallTimeout`) is
  aborted and resumed through the normal retry path; the transport has a
  30 s response-header timeout but no overall timeout (big files take
  hours).
- **Live settings:** `parallel_chunks`, `retry_max`, the global
  bandwidth limit and the base directory are atomics swapped wholesale.
  The limiter is loaded per read, so a change applies to running
  transfers immediately.
- An existing complete destination file is reported as `Skipped`; one
  clearly smaller than the expected size is downloaded again.
- Removing an unfinished item deletes its `.part`/`.part.json`
  (`DiscardPartial`; for a running transfer once it has stopped).
  Pause and shutdown keep them for resuming.
- File names pass `pathsafe.SanitizeFilename`: no directories, control
  characters or Windows-forbidden characters, no reserved device names,
  max 240 bytes.


## Plugins

Hosters are compiled in (`plugins.Hoster`), not loaded with
`plugin.Open` (Linux-only, CGO, version-locked).

```go
type Hoster interface {
    Match(url string) bool
    Resolve(url string, acc *Account) (*FileInfo, error)
    FetchDownloadURL(url string, acc *Account) (string, error)
    Name() string
    Domains() []string
}
```

Optional interfaces, detected by type assertion: `Credentialed` (needs
an account; drives the UI's account form), `DisplayNamer`,
`AccountValidator`, `Expander` (one URL → many files), `FreeDownloader`
(free-tier fallback with `ErrRetryAfter` / `ErrCaptchaRequired`),
`PremiumLinker` (buy/extend premium page).

**Matching priority (`queue.MatchHoster`).** Pass 1 considers only
plugins with a configured account, in account priority order
(`queue.RankHosters`): plugins named in the `account_priority` setting
first, then the rest with the last registered first. The multi-hosters
are registered after the native plugins (Premiumize, Debrid-Link,
AllDebrid, Real-Debrid), so by default a debrid account wins for every
host it supports, Real-Debrid first. Pass 2 considers any plugin, last
registered wins; `directhttp` (registered first) is the catch-all.
Credentialed plugins only `Match` with a configured account. The order
is read on every match (also for the hidecx resolver), so a change in
the Web UI applies to queued items.

**Falling through.** `run()` moves on to the next plugin in priority
order when one returns `ErrAccountRequired` (after trying the hoster's
other accounts, which go into cooldown) or `ErrUnsupportedHost` — "this
plugin can't serve this host right now" (not supported, down, per-host
quota), which does *not* put the account into cooldown. With no plugin
left, `ErrUnsupportedHost` is a transient failure (automatic retry).

**Multi-hosters (`plugins/internal/multihost`).** Real-Debrid predates
it; AllDebrid, Debrid-Link and Premiumize share a host-list cache (1 h,
stale list kept on a failed refresh, cleared on an invalid key), a
rate-limited client with 429 retries and a 10-minute per-link result
cache. Debrid-Link and Premiumize have no separate "check" endpoint, so
`Resolve` already generates the link and `FetchDownloadURL` reuses it.
AllDebrid's "delayed" links are polled every 5 s for up to 2 min.

**API research status.** Hoster APIs were verified as far as possible:

| Plugin | Verified | Unverified |
| --- | --- | --- |
| realdebrid | endpoints, auth, `/hosts/status`, free-account `/user` shape | error code 9 = traffic exhausted |
| rapidgator | v2 API paths/params (community clients), "Session doesn't exist" wording | `premium_end_time` type, free-tier `/file/download` POST shape |
| onefichier | base URL, auth header, `file/info`, `download/get_token`, `user/info` | premium-expiry field name, free-tier `delay` |
| katfile | full API incl. two-key auth, error envelope (errors use HTTP 200), date format | — |
| ddownload | full API endpoint list; no download endpoint exists | `status: 451` meaning |
| hidecx | container + link JSON API, required headers | other `access_status` values |
| alldebrid | docs; live: public `/v4/hosts`, error envelope | success bodies of `user/hosts`, `link/infos`, `link/unlock`, `link/delayed`, `user` |
| debridlink | docs; live: public `/downloader/hosts`, error envelope (HTTP 401 `badToken`) | success bodies of `downloader/add`, `account/infos` |
| premiumize | docs; live: public `/services/list`, error envelope (HTTP 200) | success bodies of `transfer/directdl`, `account/info` |

**rg.to** is Rapidgator's short-link domain: `rapidgator.Domains()`
lists it, and `realdebrid.Match` treats it as supported when
`rapidgator.net` is (Real-Debrid never lists `rg.to` itself).

**Referral links.** `plugins/referral.go` holds the developer's
affiliate link per hoster (empty by default); `PremiumLink` falls back
to the plain premium page and respects `accounts.referral_links`.

## Archive extraction

`internal/extract` extracts one archive and knows nothing about
packages: zip/tar.gz/tar.bz2 with the standard library, RAR (incl.
multi-part) via an external `unrar` (the Docker image ships it).
Passwords are tried as: none → package password → user list from the
settings (no built-in list). zip and tar extraction stop when their
context ends (extraction timeout, shutdown), after 100 000 entries, or
when the output exceeds 100 times the archive's size (at least 1 GiB) —
a decompression bomb, not a download; a file left unfinished is
removed.

`queue.Manager.maybeExtract` decides *when*: on every completion it
checks whether all items of the item's archive group in the same
package are done, deduplicated per group (`extractTriggered`). Before
starting it waits until every part is visible on disk
(`extract.part_wait_attempts`/`part_wait_interval`, for slow NAS
mounts). The outcome is set on every item of the group. When it does
*not* extract, it logs why: auto-extract off, parts paused or failed
(they won't finish on their own), or the first part missing from the
package; parts merely still downloading are logged at debug level.
As soon as one part is downloaded, the group's items become
`extract_pending`: a downloaded archive only counts as finished once it
is extracted, so "Clear done" and the Finished view leave it alone.
Only one archive group extracts at a time (`Manager.extractSlot`, like
a download slot): unrar already uses every core, and parallel
extractions on one volume are slower in total. A group waiting for the
slot shows `extract_queued`; the 30-minute `extractTimeout` only starts
once it extracts. Extractions interrupted by a restart, running or
queued, become `extract_failed`.

`POST /queue/{id}/extract` (`RetryExtraction`) extracts on request: a
finished archive that was never extracted (auto-extract was off when it
finished — enabling it later doesn't revisit finished items) or one
whose extraction failed, optionally with a one-off password that is
never persisted. Every part of the set must be done (`409` otherwise),
and the request claims the group in `extractTriggered` like the
automatic path, so a double click or a request during a running
extraction gets `409` instead of a second `unrar` on the same files.

## Click'n'Load

`internal/clicknload` implements the JDownloader protocol:

| Endpoint | Purpose |
| --- | --- |
| `GET /flash/` | probe; body must contain "JDownloader" |
| `GET /jdcheck.js` | probe; `jdownloader=true; var version='1.0';` |
| `POST /flash/add` | plain `urls` (CRLF-separated), `passwords`, `source`, `package` |
| `POST /flash/addcrypted2` | AES-128-CBC, key = IV = hex from the `jk` JS snippet, base64 `crypted` |

- Senders pad the plaintext with NUL bytes (JDownloader decrypts with
  `NoPadding`); PKCS7 is also accepted.
- Every response carries permissive CORS headers and `OPTIONS` is
  answered before the trust check — button scripts on HTTPS pages call
  the listener cross-origin; whether a request may *queue* anything is
  decided only by `clicknload.trusted_cidrs` (IPv4-mapped IPv6 is
  unwrapped first; an unparseable address, e.g. with an IPv6 zone, is
  rejected).
- Everything queued here starts **paused**, regardless of
  `auto_start_imports` and of the `Origin` header — deliberately no
  allowlist. The trust check only sees the client IP, and for a local
  browser that is trusted whichever page sent the request (a form POST
  needs no CORS preflight), so any website could otherwise start
  downloads. The user starts the items in the Web UI.
- Package naming: the `package` field, else the release name from the
  file names; a referring site's hostname never names a release.

## DLC containers

1. The last 88 characters of the file are the key; the rest is the
   base64 container.
2. The key is exchanged at `service.jdownloader.org/dlcrypt/service.php`
   (the only fixed outbound call Hopper makes).
3. Stage 1: `AES-CBC(key=cb99b5cbc24db398, iv=9bc24cb995cb8db3)` on the
   first 16 bytes of the base64-decoded response → raw 16-byte key, no
   unpadding.
4. Stage 2: `AES-CBC(key=iv=stage1)` on the container → strip PKCS7 →
   trim trailing NULs → base64-decode → XML.
5. Every XML text and attribute node is itself base64.

## Accounts

Credentials are AES-256-GCM encrypted at rest (`accounts.encryption_key`,
32 bytes). Validation results (valid, expiry, traffic, label) are cached
next to them; list endpoints never call the hoster. Validation runs on
add (best effort), on "Re-check", on credential update
(`PUT /accounts/{id}`) and every 12 h in the background. A confirmed
Real-Debrid token pre-warms its `/hosts/status` cache. Token-style
plugins read `Extra[...]` first and fall back to `Password`, because the
Web UI's form has only username/password.

**Rotation.** With several accounts per hoster, `AccountFor` skips those
cached as invalid, expired or out of traffic and prefers a confirmed
valid one with the most traffic left. If none qualifies it still returns
one — plugin `Match` and `MatchHoster` treat "has an account" as
"configured". When a hoster rejects an account during `run()`
(`ErrAccountRequired`), the queue reports it (`plugins.Account.ID`,
optional `AccountFailureReporter`): 30 minutes in-memory cooldown
(`cooldown_until` in the API), and the same plugin is retried with the
next account before falling back to another plugin or the free download.
A valid re-check, a credential update or re-enabling ends the cooldown.

## API, auth and Web UI

- JWT (HS256 only) from a bcrypt admin password; `server.token_ttl`
  (24 h) is the only expiry. `POST /auth/refresh` extends a *valid*
  token. Login has its own per-IP budget (5, then 10/min); failed
  attempts are logged with the client IP (fail2ban-friendly).
- The WebSocket authenticates via `Sec-WebSocket-Protocol: hopper.bearer,
  <jwt>` (not the URL, which ends up in proxy logs; `?token=` remains a
  fallback), closes when the token expires and accepts at most 4 KiB
  frames.
- Per-IP rate-limiter entries are evicted after 10 idle minutes.
- Bulk cancel is one request (`DELETE /queue`), so clearing many items
  can't trip the per-client rate limiter.
- `GET /version` and `/healthz` are unauthenticated.
- The Web UI keeps items and packages in state fed by the WebSocket;
  every (re)connect triggers a full reload, since events are not
  replayed. A 401 logs out only when *every* request of a batch failed;
  429 never does.
- Theme: dark by default, light via `prefers-color-scheme` or the
  topbar toggle (stored per browser).
- "Finished" lists packages whose items are all done (newest first, with
  path and extract result), not single done files.
- The filter tabs count packages (the rows a tab lists); file counts are
  in the stat cards and each package header.
- Update check: `internal/update` asks GitHub's latest-release endpoint
  (no drafts or pre-releases) — daily while `update_check` is on (off by
  default; hourly tick, check when the last success is over 24 h old),
  or on `POST /update/check`. The result lives in memory only; `GET
  /update` never contacts GitHub. The release link is built from the
  validated tag, not taken from the response, and manual checks are
  throttled to one a minute (GitHub allows 60 unauthenticated requests
  an hour). A "dev" build is never reported as outdated.
- Sharing from a phone: `web/public/manifest.json` declares a
  `share_target` (GET `/` with `share_url`/`share_text`/`share_title`;
  the title is kept only if it contains a URL). `lib/share.ts` parks the
  parameters in sessionStorage so they survive the login; the queue page
  puts them into the add field, nothing is added without confirmation.
  Installing the PWA needs HTTPS; iOS has no web share target, so the
  docs describe a Shortcut opening `/?share_text=...`.

## hopperctl

- `proxy` serves `127.0.0.1:9666` with HTTPS **and** plain HTTP on the
  same port (first byte `0x16` = TLS). HTTPS is needed because HTTPS
  pages may not call plain HTTP (mixed content); plain HTTP because
  `<script src="http://127.0.0.1:9666/jdcheck.js">` probes arrive
  unencrypted. The self-signed certificate is generated once and reused;
  the browser must accept it once per profile.
- `watch` polls a folder every second (no fsnotify dependency) and
  imports new `.dlc`/`.txt` via `/api/v1/import`. Files present at
  start are ignored.
- `hopperctl.yaml` holds `target` (CnL, port 9666) and `api_target`
  (API, 9667; derived from `target` when empty) — two listeners with
  different auth. The login session (JWT) is stored separately.
- `service install` registers systemd `--user`, launchd, or a real
  Windows Service (`sc.exe`, `golang.org/x/sys/windows/svc`). The
  Windows service runs as LocalSystem, so its config and logs live in
  `%ProgramData%\hopper\`; install writes the config there and to the
  user's `%APPDATA%\hopper\`.

## Browser extension

MV3, Chrome/Edge and Firefox 128+ (`background` declares both
`service_worker` and `scripts`). A MAIN-world content script overrides
`fetch`/`XMLHttpRequest` for `127.0.0.1:9666` and answers the probes
itself, so no request reaches the network (works in Brave too);
`declarativeNetRequest` redirects are the fallback. The target URL's
host is used with port 9666 for CnL and 9667 for imports; the API token
is stored in `chrome.storage.local`, never synced. Firefox requires an
AMO signature (`release.yml` signs when `AMO_API_KEY`/`AMO_API_SECRET`
are set).

## Deployment

- Image: `debian:12-slim` (needs `unrar` from non-free), multi-arch
  (amd64 + arm64 built natively), `ghcr.io/simuuh/hopper-downloader`.
- Zero-config start: without `hopper.yaml`, defaults plus `HOPPER_*`
  variables apply. Unset JWT secret, encryption key and admin password
  hash are generated on first start and kept in `secrets.json` next to
  the database (0600); explicit values always win and are never written
  there. The generated admin password is printed once to stderr (not the
  structured log); `hopperd -set-admin-password` replaces it.
  `-healthcheck`/`-set-admin-password` use `config.Peek` and never
  create files — they can run as another user via `docker exec`.
- `docker/entrypoint.sh`: the image starts as root, hands `/config`
  (recursively) and `/downloads` (only the directory itself, only if
  root-owned) to `PUID:PGID` (default 1000), then `setpriv`s to that
  user. Files written by `docker compose exec` (root) are fixed on the
  next start. `chown` failures are warnings; `PUID=0` is refused; with
  `--user` the step is skipped.
- Distribution without git (same pattern as folder-mover):
  `docker-compose.yml` and `.env.example` in the repository root are
  fetched straight from `main` (and attached to every release). The
  compose file is never edited; all settings go into `.env`, which
  Compose reads for `${VAR}` substitution *and* passes to the container
  via an optional `env_file`. Updates: `docker compose pull`.
- Naming rule for that shared `.env`: compose-only variables (`PUID`,
  `PGID`, `IMAGE_TAG`, `WEB_PORT`, `CNL_HOST`, `CNL_PORT`, `CONFIG_DIR`,
  `DOWNLOADS_DIR`) never start with `HOPPER_`; every `HOPPER_*` line is a
  hopperd setting. A compose variable named like a hopperd setting would
  silently reconfigure hopperd inside the container (it happened with
  `HOPPER_DOWNLOADS_PATH`, and the host port used to be `HOPPER_PORT`).
  `internal/config/compose_env_test.go` enforces the rule.
- `docker/docker-compose.yml` builds from a checkout with the same
  variables (in `docker/.env`) and tags the image `:dev` so it doesn't
  replace a pulled `:latest`; an existing `secrets.env` is still read.
- Click'n'Load from the host reaches the container from Docker's bridge
  network (172.x), not 127.0.0.1 — the Compose files therefore trust the
  private ranges inside the container; reachability is controlled by the
  host-side port binding (`CNL_HOST`, default 127.0.0.1).
- Compose interpolates `$` in `.env` values unless they're single-quoted
  — a bcrypt hash must be written `'…'` (or with every `$` as `$$`).
- Empty `HOPPER_NOTIFY_*` values don't override `hopper.yaml` (an empty
  line in `.env` arrives as "" in the container).
- Config: `hopper.yaml` with `${VAR}` expansion, `HOPPER_*` overrides
  win.

## Security model

- Every `/api/v1` route except login/version requires a JWT.
- An authenticated caller may queue any URL, including internal hosts —
  deliberate (homelab targets), the same trust boundary as curl.
- Click'n'Load is loopback-only by default (`trusted_cidrs`) and never
  starts a download by itself: its links are always queued paused.
- Secrets in URLs (Rapidgator's token query parameter) are redacted from
  transport errors before they reach logs or the UI.
- Strict CORS for the API, CSP and security headers on every response,
  per-client rate limiting.

## Known gaps

- DDownload can't download without Real-Debrid (no API endpoint,
  interactive Turnstile on the website).
- Katfile and Rapidgator free-tier flows are unverified/not implemented
  for scraping-based downloads.
- 1fichier free tier has no captcha support.
- A Windows service needs a login session readable by LocalSystem for
  folder-watch imports.
