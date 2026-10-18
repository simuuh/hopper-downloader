# Using Hopper

- [Adding links](#adding-links)
- [From a phone](#from-a-phone)
- [Packages](#packages)
- [Hoster accounts](#hoster-accounts)
- [Notifications](#notifications)

## Adding links

| Way | How |
|---|---|
| Paste | Paste any text containing links into the add field. Parts of a release (`movie.part1.rar` … `partN.rar`) become one package; later parts merge into it; duplicates and mirrors are skipped. |
| `.dlc` / `.txt` | Drop the files anywhere on the Web UI, or let the browser extension / `hopperctl watch` import them. |
| Click'n'Load | Directly if hopperd runs on your PC; with hopperd on a NAS via the browser extension or hopperctl — see [REMOTE_CLICKNLOAD.md](REMOTE_CLICKNLOAD.md). |
| Phone | Share the link to Hopper — see below. |
| API | `POST /api/v1/links/bulk` — see [API.md](API.md). |

`.dlc` / `.txt` imports start right away; turn off "Start imported .dlc/.txt
links immediately" in Settings to add them paused for review first.

Click'n'Load links are always added paused, regardless of that setting —
any web page you open could send links to the Click'n'Load listener, so
nothing it adds downloads until you start it in the Web UI.

## From a phone

Click'n'Load can't work on a phone. Instead, share the link to Hopper:
it opens with the link in the add field, you check it and press Add.

**Android (Chrome, Edge, Samsung Internet):** open the Web UI and
"Install app" / "Add to Home screen". Hopper then shows up in the share
sheet of every app. Installing needs the Web UI served over **HTTPS**
(e.g. behind your reverse proxy) — browsers don't install apps from plain
`http://` LAN addresses.

**iOS, or without HTTPS:** iOS has no share target for web apps. Create a
Shortcut (Shortcuts app → new shortcut → "Show in Share Sheet", accepts
URLs and text) with one action, *Open URLs*:

```
https://hopper.example.lan/?share_text=[Shortcut Input]
```

The same URL works from any automation or bookmarklet:
`/?share_url=<link>` or `/?share_text=<text with links>`. If you're not
signed in yet, the links wait until you are.

## Packages

- **Finished** lists packages whose downloads are all done, newest
  first, with finish time, target folder and extraction result. A
  package with files still downloading only shows up there once it's
  complete.
- **+** on a package adds links to exactly that package — a missing part,
  a working mirror. Duplicates are skipped.
- **✎** on a queued, paused or failed file changes its URL or filename.
  A new URL restarts a failed download, e.g. after its link died.
- "Save to…" (before adding) or the package path sets a subfolder below
  the downloads directory.
- With `extract.enabled`, archives are extracted once all parts are on
  disk; parts download in order (`queue.sequential_parts`). Failed
  extraction (e.g. wrong password) can be retried from the file row.
  An archive that finished while auto-extract was off shows "Extract
  now" on its first part. If an archive isn't extracted, hopperd's log
  says why (`queue: auto-extract is off …`, `queue: extract waits for
  parts that won't finish on their own …` — start or retry those parts).

## Hoster accounts

Add accounts under Accounts. Credentials are encrypted at rest and
re-validated every 12 hours (expiry, traffic left).

With several accounts for one hoster, Hopper uses the best one: accounts
known to be invalid, expired or out of traffic are skipped, and the one
with the most traffic left is preferred. If a hoster rejects an account
during a download, Hopper switches to the next one immediately and pauses
the rejected account for 30 minutes ("paused until …" in the list) — a
successful Re-check ends that early.

### Debrid services and priority

Real-Debrid, AllDebrid, Debrid-Link and Premiumize.me ("debrid" or
multi-hoster services) each cover many hosters with one API key. When
several of your accounts can download a link — two debrid services, or a
debrid service and a native Rapidgator account — Hopper uses the one
highest in **Accounts → Priority** (shown once two plugins have an
account). Default order: Real-Debrid, AllDebrid, Debrid-Link,
Premiumize.me, then the native plugins.

If the chosen service can't serve the link right now — host not
supported, down, or its daily limit for that host reached — Hopper tries
the next one in the list without pausing the account (it's fine, just
not for this host). A rejected or exhausted account is paused as
described above.

## Notifications

**Push (server):** hopperd notifies when a package is finished — all
files downloaded or failed, and extraction done if it applies. It works
with no browser open.

Docker: in the `.env` next to `docker-compose.yml`

```sh
HOPPER_NOTIFY_WEBHOOK_URL=http://homeassistant.lan:8123/api/webhook/hopper
HOPPER_NOTIFY_NTFY_URL=https://ntfy.sh/my-hopper-topic
#HOPPER_NOTIFY_NTFY_TOKEN=         # only for protected topics
```

then `docker compose up -d`. Without Docker: the same variables, or
`notify.webhook_url` / `notify.ntfy_url` / `notify.ntfy_token` in
`hopper.yaml`.

The webhook receives a JSON POST:

```json
{
  "event": "package_finished",
  "package_id": "84de3e73…",
  "package": "movie",
  "status": "completed",
  "items": 3,
  "done": 3,
  "failed": 0,
  "bytes": 900000,
  "extract": "extracted",
  "path": "movie",
  "finished_at": "2026-10-16T19:38:31+02:00"
}
```

`status` is `completed`, `partial` (some files or the extraction failed)
or `failed` (nothing downloaded). `path` is relative to the downloads
directory — enough to hand the folder to a script or a media server. To
sort finished downloads into a movie/series library, use
[Folder Mover](https://github.com/simuuh/folder-mover) with its
`download_dir` set to Hopper's downloads directory. ntfy gets a short title and message, with high priority
for partial/failed packages. Settings → Push notifications has a test
button.

**Browser:** Settings → This browser enables desktop notifications while
the Web UI is open.
