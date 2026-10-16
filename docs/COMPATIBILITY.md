# Compatibility

What works with which service, as observed against the real services —
not a guarantee for anything not listed. Implementation details are in
[ARCHITECTURE.md](ARCHITECTURE.md).

## Hosters

| Hoster | Plugin | Download | Account | Notes |
| --- | --- | --- | --- | --- |
| Real-Debrid | `realdebrid` | ✅ | API token | Multi-hoster: claims every host listed in its live `/hosts/status`. |
| AllDebrid | `alldebrid` | ✅¹ | API key | Multi-hoster: claims the account's own host list (`/v4.1/user/hosts`), minus hosts marked down or out of quota. Refuses requests from server/VPN IPs (`NO_SERVER`) — the next service takes over. |
| Debrid-Link | `debridlink` | ✅¹ | API key | Multi-hoster: claims online file hosts from `/downloader/hosts`. Exceeding its unpublished rate limit blocks the key for ~1 h. |
| Premiumize.me | `premiumize` | ✅¹ | API key | Multi-hoster: claims services with instant download (`directdl` in `/services/list`, 20 services as of October 2026). Rapidgator and Katfile are only offered as asynchronous transfers there (`queue`), which Hopper doesn't use — they go to the next service. Fair-use quota instead of traffic. |
| Rapidgator (`rapidgator.net`, `rg.to`) | `rapidgator` | ✅ | username + password | `rg.to` is Rapidgator's own short-link domain. Free-tier fallback is implemented but unverified against a live free account. |
| 1fichier | `onefichier` | ✅ | API key | Free-tier wait (`delay`) handled; no captcha support. |
| Katfile (`katfile.cc`, `katfile.biz`) | `katfile` | ✅ | `key1` + `key2` | API has no anonymous access at all — an account is required even for metadata. |
| DDownload | `ddownload` | ⚠️ metadata only | API key | The API has no download-URL endpoint and the website requires an interactive Cloudflare Turnstile challenge. Download through a debrid service instead (Real-Debrid, Debrid-Link). |
| Any direct HTTP(S) link | `directhttp` | ✅ | — | Catch-all for everything no other plugin claims. |

¹ Implemented against the official API docs; host lists and error
responses were checked live, the success responses not yet with a real
account.

A hoster plugin only claims a URL when an account for it is configured.
With several accounts that can handle a URL, the account priority
decides (Accounts → Priority; default: Real-Debrid, AllDebrid,
Debrid-Link, Premiumize.me, then native plugins); without any, the URL
falls through to `directhttp`.

## Link protectors

| Service | How links arrive | Notes |
| --- | --- | --- |
| filecrypt.cc | Click'n'Load 2 (`/flash/addcrypted2`, AES) or a `.dlc` downloaded in the browser | Container pages require a captcha solved in a real browser. Hopper never fetches them itself — the browser's Click'n'Load button (or the `.dlc` you download) hands the links over. `/DLC/<id>.dlc` returns an empty body without a captcha-solved session. |
| hide.cx | Click'n'Load (`/flash/add`) or pasted container URL | Not a hoster: a container (`hide.cx/container/<uuid>`) wraps one or more real hoster links behind a JSON API. The `hidecx` plugin expands it into one queue item per part, each matched against all other plugins. |
| Other CnL-capable sites | Click'n'Load 1/2 | Any page with a JDownloader-compatible Click'n'Load button works the same way. |

## Browsers

- **Chrome / Edge / Firefox 128+** — Click'n'Load works with the browser
  extension, or with `hopperctl proxy` after accepting its certificate
  once (`https://127.0.0.1:9666/flash/`).
- **Brave / hardened Firefox** — Shields or strict tracking protection
  can block a page's request to `127.0.0.1:9666`. Use the extension,
  allow the site, or import the `.dlc` instead. See
  [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
- **Phones / tablets** — Click'n'Load targets `127.0.0.1` on the device
  running the browser, so it cannot reach a NAS from a phone. Share the
  link to Hopper instead (see [USAGE.md](USAGE.md#from-a-phone)), paste
  it, or import a `.dlc` in the Web UI.
