# Troubleshooting

Common problems and their actual root cause, not just the workaround —
see ARCHITECTURE.md for the full investigation behind each of these if you want
more detail than fits here.

## Extraction runs forever or shows "Extracting…"

Since the progress-logging fix, hopperd logs `queue: extract still
running` with an elapsed time every 60s while a real extraction is in
progress — check hopperd's logs first to tell "genuinely still running" (a
large multi-part RAR set can legitimately take many minutes) apart from
"actually stuck".

**If hopperd restarted while an item showed "Extracting…":** that
extraction was killed along with the process that started it. On the next
startup, `queue.Manager` automatically resets any item still in that
state (or waiting in "Waiting to extract…") to `extract_failed` with the message "extraction interrupted by
restart — use re-extract button" — you don't need to do anything but
click that button (`ItemRow`'s archive icon) to retry. If you're seeing a
persistently stuck "Extracting…" item that *didn't* recover after a
restart, that's unexpected — check hopperd actually restarted (its
startup log line) rather than just the Web UI reconnecting.

**If it fails with "not all archive parts are present on disk":** the
downloader reported every part done, but the files weren't all visible on
disk yet when extraction checked — a network-mounted `/downloads` volume
(NAS, Docker bind mount over a slow connection) is the realistic cause.
Raise `extract.part_wait_attempts`/`part_wait_interval` in `hopper.yaml`
(defaults: 10 attempts, 500ms apart — up from an original 5×200ms that
proved too short in practice) and retry.

## CnL links not all arriving

hopperd now logs a per-URL reason for anything it skips
(`clicknload: skipping invalid url`) and a summary line
(`clicknload: accepted N/M links`) for every submission — check hopperd's
logs for exactly which URL(s) were dropped and why before assuming
something is broken.

If *nothing* arrives at all, work through these in order:

1. **CORS / Mixed Content** — a hoster's Click'n'Load button lives on an
   HTTPS page and POSTs to `hopperctl proxy` or the browser extension's
   target. `hopperctl proxy` serves both HTTP and HTTPS on the same
   `127.0.0.1:9666` (see ARCHITECTURE.md — some
   hoster pages, e.g. filecrypt.cc, load `jdcheck.js` via a plain
   `<script src="http://...">` tag rather than fetch/XHR, which the
   HTTPS-only POST side alone can't answer); the first time you use the
   HTTPS side in a given browser profile, open
   `https://127.0.0.1:9666/flash/` directly and click through the
   certificate warning once — this is a genuine one-time step, not
   something the tooling can skip (see ARCHITECTURE.md for
   why browsers require it even for a self-signed localhost cert).
2. **trusted_cidrs** — `hopperd`'s own CnL listener (or `hopperctl
   proxy`'s target) rejects any client IP not in `clicknload.trusted_cidrs`
   with a 403, logged as `clicknload: rejected untrusted client`. Widen
   it if you're relaying from a different host/subnet than the default
   `127.0.0.1/32` covers.
3. **Brave / hardened Firefox** — see below; this is a known limitation,
   not a bug to chase.

## Downloads: permission denied

The container makes `/config` and the downloads folder writable for
`PUID:PGID` (default 1000:1000) on every start — check its log
(`docker compose logs hopperd`) for an `entrypoint: warning`:

- **`/downloads is not writable for …`** — you mounted an existing folder
  owned by another user. Set `PUID`/`PGID` in `.env` to its owner
  (`stat -c %u:%g <folder>` on the host) and `docker compose up -d`.
  Hopper deliberately never changes ownership inside an existing folder.
- **`could not change ownership …`** — the filesystem refuses `chown`
  (NFS with root_squash, some NAS shares). Make the host folder writable
  for `PUID:PGID` there.

Running the container with `user:`/`--user` skips this step; then the
host folders must already belong to that user. Running hopperd directly
(not via Docker): the process's own user must own `downloads.path` and
the directory of `database.path`.

## Brave (or a hardened Firefox) blocks Click'n'Load

Known limitation, not a bug: Brave's Shields (and similarly configured
Firefox privacy extensions) block a page's request to a local address
like `127.0.0.1:9666` even over the trusted HTTPS certificate described
above. There is no application-side fix — either disable Shields for the
specific hoster site, or use a browser that doesn't block localhost
requests by default (Edge/Chrome/plain Firefox) for Click'n'Load. A `.dlc`
file import (drag onto the Web UI, or `hopperctl watch`'s folder watcher)
works regardless, since it doesn't depend on a page's JS reaching a local
listener at all.

## Folder-Watch (`hopperctl watch`) not importing files

`hopperctl watch`/`run` needs two independent things configured
correctly — `hopperctl status` reports both:

- **A logged-in session** (`hopperctl login`) — its token is what
  authenticates the `POST /api/v1/import` call. No session means watch
  is disabled with a logged warning, not a hard failure (a CnL-only relay
  with no folder watch is a valid setup).
- **`api_target`** (or a `target` it can derive one from) pointing at
  hopperd's *API* port (9667), not its CnL port (9666) — these are
  deliberately separate fields in `hopperctl.yaml` because the CnL relay
  and the JWT-authenticated import call are two genuinely different
  destinations with different auth requirements. If `target` points at
  a NAS's CnL relay and you never set `api_target` explicitly, confirm
  the derived value (`hopperctl status` shows both the resolved
  `api_target` and whether it was derived) is actually the same hopperd
  instance you intend to import into.

If both check out and files still aren't imported, confirm the watched
directory (`watch_dir` in `hopperctl.yaml`, or `-dir`) actually matches
where your browser saves `.dlc`/`.txt` downloads, and that the files
weren't already present (and therefore already recorded as "seen") before
the watcher started — see `hopperctl`'s log line on startup listing the
directory it's watching.

## Verifying your Hopper setup end-to-end

The automated tests cover the logic; these steps need a real browser, a
real service install or real files, so they're manual. Work through the
sections that apply to your setup.

### Browser extension

- [ ] Load it via `chrome://extensions` → Developer mode → Load unpacked
      → `extension/`.
- [ ] The popup shows a green dot and hopperd's version once the target
      URL (and, for imports, an API token) is configured.
- [ ] On any page with a Click'n'Load button (e.g. a filecrypt.cc
      container after solving its captcha) the button activates, and
      clicking it adds the links to the queue, grouped per release.
- [ ] Clicking the same button again adds nothing new — the Web UI
      reports the links as duplicates.

### hopperctl proxy / Windows Service

- [ ] Open `https://127.0.0.1:9666/flash/` once per browser profile and
      accept the self-signed certificate.
- [ ] `hopperctl status` reports both `proxy: reachable` and
      `target: reachable`.
- [ ] Windows: `sc query HopperCtl` → `RUNNING`;
      `%ProgramData%\hopper\hopperctl.log` shows `proxy listening
      (HTTPS)` without `no target configured` warnings.
- [ ] Click'n'Load in a browser *without* the extension works purely
      through the proxy.

### hopperctl folder watch

- [ ] Save a `.dlc` into `watch_dir`; within ~2 s its links appear in
      the queue and `hopperctl.log` logs the import.
- [ ] With `delete_after: true` the `.dlc` is removed afterwards.

### Hoster accounts

- [ ] Accounts → add an account → status turns ✅ with an expiry date
      (or a clear "free account" / "traffic exhausted" label).
- [ ] A DDownload link resolves (name, size) but fails at download time
      with "no working download-URL mechanism" unless Real-Debrid is
      configured — expected, see [COMPATIBILITY.md](COMPATIBILITY.md).
