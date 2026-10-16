# Writing a Hopper hoster plugin

Hopper's hoster plugins are ordinary Go packages compiled into `hopperd`,
not dynamically loaded modules. Go's native `plugin` package (`plugin.Open`)
is Linux-only, ties the plugin binary to the exact toolchain version that
built the host, and can't produce the Windows/macOS artifacts Hopper
ships — so it isn't used. Adding a hoster means writing a package and
registering it in `cmd/hopperd/main.go`.

## The interface

```go
// github.com/simuuh/hopper-downloader/plugins/plugin.go

type Hoster interface {
    Match(url string) bool
    Resolve(url string, acc *Account) (*FileInfo, error)
    FetchDownloadURL(url string, acc *Account) (string, error)
    Name() string
    Domains() []string
}
```

- **Match** — cheap, no network I/O. Usually a hostname check.
- **Resolve** — look up file metadata (name, size, availability) without
  downloading. `acc` is `nil` when the download queue has no account
  configured for this hoster's key; return `plugins.ErrAccountRequired`
  if that means you can't proceed.
- **FetchDownloadURL** — exchange the page URL for a direct URL the
  downloader can issue `Range` requests against.
- **Name** — must be a stable, unique identifier (e.g. `"rapidgator"`).
  It's used as the hoster key for accounts and per-hoster bandwidth/rate
  limits in `hopper.yaml`.
- **Domains** — hostnames this plugin owns, for documentation/UI use.
  `Match` is what actually decides routing.

Return the sentinel errors in `plugins/plugin.go`
(`ErrNotFound`, `ErrUnavailable`, `ErrAccountRequired`, `ErrRateLimited`)
where they apply — the queue manager and API react to them specifically
rather than parsing error strings.

## Reference implementations

- **`plugins/directhttp`** — the template. No hoster-specific auth; a
  `HEAD` request for `Resolve`, and `FetchDownloadURL` returns its input
  unchanged. Copy this file as your starting point.
- **`plugins/rapidgator`** — a fully-implemented, single-token
  credentialed plugin backed by a real login-exchange session (cached,
  re-obtained reactively on `ErrAccountRequired` — see its
  `cachedSession`/`callWithToken`).
- **`plugins/katfile`** — a two-credential (`key1`+`key2`, not a single
  token) plugin: `/authorize` exchanges both for a short-lived
  `access_token`+`account_id` pair, cached and proactively refreshed
  before its documented ~1h expiry (`sessionFor`/`tokenMaxAge`) as well
  as reactively on a "please reauthenticate" response
  (`callAuthorized`). Worth reading if your hoster's auth is a two-step
  exchange rather than one bearer token, or if its success/error JSON
  responses use two genuinely different envelope shapes that need to be
  told apart *before* decoding (`successEnvelope`/`errorEnvelope`) rather
  than one shape with an error field that's simply empty on success.
- **`plugins/ddownload`** — a plugin whose `FetchDownloadURL` is a
  deliberate, documented stub: real investigation (live requests against
  the actual API and website) found no working direct-download
  mechanism at all — see its doc comment for what was tried and ruled
  out. Worth reading as the template for "this hoster's Hoster
  interface can be implemented, but one required method genuinely can't
  be — say so clearly, with the evidence, instead of guessing an
  endpoint and shipping it as if confirmed."
- **`plugins/realdebrid`** — a fuller example: an account-gated plugin
  backed by a third-party "debrid" API rather than a hoster directly.
- **`plugins/alldebrid`, `debridlink`, `premiumize`** — further
  multi-hosters built on `plugins/internal/multihost` (host-list cache,
  rate-limited client, per-link result cache). A multi-hoster that can't
  serve a URL right now (host unsupported/down, per-host limit) returns
  `plugins.ErrUnsupportedHost` so the queue tries the next plugin in the
  account priority; `ErrAccountRequired` is for the account itself
  (invalid, not premium, exhausted).
  Worth reading if your plugin needs any of: a bearer-token account
  (`acc.Extra["api_token"]`), a self-refreshing cache (`/hosts/status`,
  polled at most once/hour) to decide `Match` instead of a static domain
  list, a client-side rate limiter plus HTTP 429 retry, or `Match`
  reaching for account state itself since `Match(url string) bool` takes
  no `*Account` (unlike `Resolve`/`FetchDownloadURL`) — see its
  `TokenSource` interface for the pattern.
- **`plugins/hidecx`** — a link-redirector plugin that never serves a
  file itself: it queries the redirector's own container API (not
  necessarily an HTTP redirect chain — hide.cx's real backend is a JSON
  API on a separate subdomain, confirmed by fetching a live response
  rather than assumed) and *delegates* to whichever other registered
  plugin claims the real URL underneath. Worth reading if your plugin
  needs to re-run plugin matching on a URL it doesn't own (e.g. another
  redirector/container format) — see its `Resolver` closure, injected
  from `cmd/hopperd/main.go` the same way the settings/queue circular
  dependency is resolved (a `var registry *plugins.Registry` declared
  before the plugin needing it is constructed, assigned only once
  `plugins.NewRegistry` has actually run), and `queue.MatchHoster`, the
  exported form of the queue
  manager's own account-aware two-pass matching, reused rather than
  reimplemented. It also implements `Expander` (below) — one hide.cx
  container can hold every part of a multi-part archive, not just one
  file.

## Optional interfaces

Beyond the required `Hoster` methods, a plugin can opt into extra
behavior by implementing any of these — all checked via type assertion
(`h.(SomeInterface)`), so a plugin that doesn't implement one is treated
as if it simply doesn't have that capability, never an error:

- **`Credentialed() bool`** — needs a configured account to function at
  all (see rapidgator's stub vs. onefichier/realdebrid). Gates whether
  the plugin shows up in the Web UI's "add account" dropdown.
- **`DisplayName() string`** — a nicer Web UI label than `Name()`.
- **`AccountValidator`** (`ValidateAccount(*Account) (*AccountInfo,
  error)`) — check a configured account against the hoster's own API
  (see realdebrid's `GET /user`).
- **`FreeDownloader`** (`FetchFreeDownloadURL(ctx context.Context, url
  string, acc *Account) (string, error)`) — a fallback download path for
  when the account the queue layer resolved *is* configured but turns
  out not to be premium/full-access (your `FetchDownloadURL` returns
  `ErrAccountRequired` for that case, same sentinel as "no account at
  all" — the queue layer is what tells the two apart, by whether a
  different plugin could pick up the URL first). `queue.Manager.run`
  only reaches this after its own plugin-fallback loop
  (`matchHosterExcluding`) finds no other plugin left to try — see
  ARCHITECTURE.md for the full
  design, including two error types your implementation can return
  instead of a plain error: `ErrRetryAfter{Seconds}` (hoster wants a
  wait) and `ErrCaptchaRequired{ImageURL}` (hoster wants a captcha
  solved — **only implement this if you have a real image URL to give
  the user**; see `plugins/onefichier`'s `FetchFreeDownloadURL` for the
  alternative when you don't: surface it as a plain error instead of
  fabricating an empty `ImageURL` that would just render a broken image).
  Not every credentialed plugin needs this — `plugins/rapidgator`,
  `plugins/onefichier`, and `plugins/realdebrid` all implement it against
  a real free/limited tier; `plugins/ddownload`/`plugins/katfile`
  deliberately don't, because live investigation found their only
  anonymous/free paths gated by mechanisms this interface (and no
  server-side Go HTTP client) can automate or honestly represent — see
  each package's own doc comment before assuming a "free download"
  feature is automatable just because the hoster's website has one.
- **`Expander`** (`Expand(url string) ([]ExpandedLink, error)`) — a
  single URL your plugin matches can actually represent *multiple*
  downloadable files (see `plugins/hidecx`: one hide.cx container can
  hold every part of a multi-part archive, a variable count confirmed
  against a live response — not one file with alternate mirrors).
  `queue.Manager` checks this *before* adding an item at all
  (`addItem`/`addItems`, called from both `AddLink` and `AddPackage`'s
  per-URL loop): if your plugin's `Match` claims a URL and it also
  implements `Expander`, `Expand` is called first, and — if it succeeds
  with at least one link — every `ExpandedLink` is queued as its own
  item under one package (a new one for `AddLink`, or the caller's
  existing one for `AddPackage`) instead of a single item for the
  original URL. Each item is then matched *again*, against its own
  `ExpandedLink.URL` (the real, already-resolved link — not the
  original container URL), so an account-holding plugin like Real-Debrid
  can still claim it exactly as if that URL had been queued directly.
  If `Expand` errors or returns nothing, the URL is queued as a single
  plain item instead (your plugin's own `Resolve`/`FetchDownloadURL` get
  a chance to handle it at run time) — expansion failing never blocks
  queueing outright.

## Redirector plugins (Expander interface)

If your plugin's URL doesn't point at a single downloadable file but at
a container page wrapping several (e.g. every part of a multi-part
archive), implement `Expander` *alongside* the required `Hoster`
methods, not instead of them: `Resolve`/`FetchDownloadURL` still need
to work as a single-link fallback for when `Expand` isn't used or
fails. `plugins/hidecx` is the reference implementation — `Expand` is
its primary path; its `Resolve`/`FetchDownloadURL` exist only as that
fallback.

Key points:

- `Expand(url string) ([]ExpandedLink, error)` returns every real,
  already-resolved hoster URL the container holds — distinct files
  (e.g. `.part1.rar` through `.partN.rar`), not alternate mirrors of
  one file.
- `queue.Manager.addItem` checks for `Expander` (via type assertion)
  *before* the normal single-item path, right after your plugin's
  `Match` claims the URL. On a successful, non-empty `Expand`, every
  `ExpandedLink` is queued as its own item and re-matched against its
  own real URL — so an account-holding plugin like Real-Debrid can
  still claim, say, a rapidgator.net URL found inside your container,
  exactly as if that URL had been queued directly. This reuses
  `queue.MatchHoster` (exported specifically for this) rather than a
  second, potentially drifting copy of its account-priority logic.
- That second match needs the full plugin registry, which doesn't
  exist yet at your plugin's own construction time — it's one of the
  registry's own members. `plugins/hidecx` resolves this with a
  `Resolver` closure injected from `cmd/hopperd/main.go`, capturing a
  `var registry *plugins.Registry` declared before `NewRegistry` runs
  and assigned only afterward — the same circular-constructor pattern
  `internal/settings.Manager`/`queue.Manager` already use (see
  ARCHITECTURE.md).
- Registration order doesn't matter for a redirector the way it does
  for overlapping native/debrid plugins: as long as it's the only
  plugin whose `Match` claims its own domain, `Registry.Match` finds it
  regardless of position. Order only matters when two plugins could
  both claim the same URL, which a redirector normally doesn't.

## Steps to add a plugin

1. Create `plugins/<name>/<name>.go` with a `Plugin` type implementing
   `plugins.Hoster`, and a `New() *Plugin` constructor. Keep it
   dependency-light — stdlib `net/http` is enough for most hosters.
2. If the hoster needs credentials, use `acc.Username`, `acc.Password`,
   and `acc.Extra` (a `map[string]string]` for anything that doesn't fit
   username/password — API tokens, session cookies).
3. Register it in `cmd/hopperd/main.go`:

   ```go
   registry := plugins.NewRegistry(
       directhttp.New(), // catch-all fallback; keep this first
       rapidgator.New(),
       yourhoster.New(),
   )
   ```

   Registration order matters: `Registry.Match` checks plugins in
   *reverse* registration order, so put more specific plugins after
   `directhttp` and they'll take priority over the catch-all.
4. Write tests against an `httptest.Server` — see
   `plugins/directhttp/directhttp_test.go` for the pattern this codebase
   uses for HTTP-fixture-based plugin tests (also see
   `internal/downloader/downloader_test.go` for the same approach at the
   transfer-engine level).
5. If the hoster has a free-tier wait time between downloads or a
   published rate limit, document it — operators set this per-hoster in
   `hopper.yaml`'s `hosters.<name>.min_request_interval` /
   `bandwidth_limit` (see `hopper.example.yaml`), keyed by your plugin's
   `Name()`.

## What the plugin does *not* need to handle

- **Chunking, resume, retry, bandwidth limiting** — `internal/downloader`
  owns the transfer once it has a direct URL.
- **Path/filename sanitization** — `internal/pathsafe` is applied by the
  queue manager before any filename you return in `FileInfo.Name` touches
  the filesystem.
- **Credential storage/encryption** — `internal/account` handles that;
  your plugin only ever sees a decrypted `*plugins.Account` in memory,
  for the duration of one call.
