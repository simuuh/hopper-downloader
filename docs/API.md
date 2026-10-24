# Hopper REST API

Machine-readable spec: [`api/openapi.yaml`](../api/openapi.yaml) (OpenAPI
3.0 — load it into Swagger UI, Redocly, Postman, etc. for interactive
docs). This page is a human-oriented overview.

## Auth

Hopper has one operator identity ("admin"), authenticated by the
password hashed into `server.admin_password_hash`. There is no
multi-user account system.

```
POST /api/v1/auth/login
{ "username": "admin", "password": "..." }

→ { "token": "<JWT>", "expires_at": "2026-01-01T00:00:00Z" }
```

Send the token on every other `/api/v1/*` request:

```
Authorization: Bearer <JWT>
```

Tokens are HS256-signed with `server.jwt_secret` and expire after
`server.token_ttl` (default 24h).

```
POST /api/v1/auth/refresh
Authorization: Bearer <still-valid JWT>

→ { "token": "<new JWT>", "expires_at": "..." }
```

Exchanges a currently-valid token for a fresh one with a full new
`token_ttl` window, without the password — an already-expired token is
rejected the same way any other request is, so this can't resurrect a
dead session, only extend a live one. The Web UI calls this proactively
before a bulk action if the stored token is close to expiring (see
`web/src/lib/api.ts`'s `tokenNearExpiry`), so a long multi-select
operation's parallel requests don't legitimately 401 partway through
just because the token expired mid-batch.

`GET /healthz` is the only other unauthenticated endpoint (used by
Docker's `HEALTHCHECK` and `hopperd -healthcheck`).

## Versioning

Everything lives under `/api/v1`. A breaking change gets `/api/v2`
alongside it, not a silent change to `/api/v1`.

## Core resources

| Resource | Endpoints |
|---|---|
| Links | `POST /links` |
| Packages | `POST /packages`, `GET /packages` |
| Queue | `GET /queue`, `GET /queue/{id}`, `PUT /queue/order`, `POST /queue/{id}/{pause,resume,retry,extract,captcha}`, `DELETE /queue/{id}` |
| Accounts | `GET /accounts`, `POST /accounts`, `PUT /accounts/{id}` (replace credentials, re-validates), `POST /accounts/{id}/enabled`, `POST /accounts/{id}/validate`, `DELETE /accounts/{id}` |
| Plugins | `GET /plugins` — credentialed hosters in account priority order (`rank`, 1 = first) incl. `premium_url` (buy/extend premium; `is_referral` marks the developer referral link, see `accounts.referral_links`) |
| Settings | `GET /settings`, `POST /settings` (partial update; `account_priority` = plugin keys, most preferred first, `[]` = default order; `update_check` = daily release check, off by default) |
| Config | `GET /config` (read-only, redacted) |
| Stats | `GET /stats` |
| Import | `POST /import` (multipart, one or more `.dlc`/`.txt` files; grouped + de-duplicated) |
| Bulk actions | `POST /queue/actions` with `{ids, action}` (`pause`, `resume`, `retry`, `delete`) — resume/retry are atomic, items start top to bottom |
| Packages | `PATCH /packages/{id}` (name, path), `POST /packages/{id}/links` (add links to it), `POST /packages/{id}/{pause,resume,retry}`, `DELETE /packages/{id}` |
| Queue item edit | `PATCH /queue/{id}` (`priority`; `url`/`filename` while queued, paused or failed) |
| Notifications | `GET /notify` (configured?), `POST /notify/test` |
| Updates | `GET /update` (last check result, cached), `POST /update/check` (check GitHub now; at most once a minute) |
| Bulk add | `POST /links/bulk` (free text with URLs; grouped per release, merged, de-duplicated) |

Queue item states: `queued → downloading → done`, with `paused` and
`error` as side branches (`pause`/`resume`/`retry`/`cancel` move between
them — see `internal/queue`'s state machine for the exact transitions).

### Reordering

`PUT /queue/order` takes `{"ids": [...]}` — the full desired order of
however many items you're showing (e.g. everything the UI currently has
rendered). IDs it doesn't mention keep their current position; this
matches what a drag-and-drop queue table naturally produces.

### Extracting an archive on request

```
POST /queue/{id}/extract
{ "password": "..." }   # optional
```

Valid for an item that's `state: "done"` and either never extracted (no
`extract_state` — auto-extract was off when it finished — or
`extract_pending`) or with `extract_state: "extract_failed"`; works whether auto-extract is on or
not. Anything else (extracting, extracted, not done) is `400`. Resets
`extract_state` to `extract_pending` and runs extraction in the background; watch `/ws` for
the item transitioning to `extracting` and then `extracted` or
`extract_failed`. `password`, if given, is tried ahead of the persisted
`extract_passwords` (see `POST /settings`) for this attempt only — it's
never written to settings. `409` if another part of the archive isn't
done yet or the archive is already being extracted; `400` if the
archive's file(s) can no longer be found on disk; `404` if the item
doesn't exist.

### Solving a free-download captcha

```
POST /queue/{id}/captcha
{ "solution": "..." }
```

An item whose configured hoster account isn't premium can fall back to
that plugin's own free-download flow (see `internal/queue`'s
`tryFreeFallback`); some hosters' free flow requires solving a captcha
first, surfaced as `captcha_url` on the item (watch `/ws` for it, same as
`extract_state`). Submitting a solution here clears `captcha_url` and
retries the download immediately — no separate "resume" call needed.
`400` if `solution` is empty or the item has no captcha currently
pending; `404` if the item doesn't exist. A wrong solution simply fails
the retry the normal way (the item's `error` field, or another captcha
challenge if the hoster issues a new one) — there's no separate "solution
rejected" status.

A related, entirely automatic case needs no endpoint at all: a hoster
asking the client to wait before retrying (rather than solve a captcha)
surfaces as `retry_after` on the item — queue.Manager schedules the retry
itself once that time passes.

## Live updates: `/ws`

WebSocket, same auth requirement, with one exception: since browsers
can't set custom headers on the WebSocket handshake, you can pass the
token as a query parameter instead of a header:

```
wss://host/ws?token=<JWT>
```

(Prefer the `Authorization` header when your client can set one — the
query-string form ends up in server/proxy access logs.)

The server pushes JSON `Event` frames as things change:

```json
{ "type": "item_updated", "item": { "id": "...", "state": "downloading", "downloaded": 1048576, "speed": 524288, ... } }
{ "type": "item_removed", "item": { "id": "..." } }
{ "type": "package_updated", "package": { "id": "...", "name": "..." } }
```

It never expects messages back from the client. Ping frames keep the
connection alive every 30s; reconnect on close (the Web UI's
`useQueueSocket` hook does this with a fixed 2s backoff).

## Errors

Every non-2xx response is `{"error": "human-readable message"}`. Notable
status codes:

- `401` — missing/invalid/expired token (or wrong login credentials)
- `404` — queue item / account not found
- `409` — queue action not valid for the item's current state (e.g.
  `pause` on an already-`done` item)
- `429` — per-client rate limit exceeded (`ratelimit.requests_per_minute` / `burst`)

## Security headers & CORS

Every response carries CSP, `X-Frame-Options: DENY`,
`X-Content-Type-Options: nosniff`, HSTS, and a `no-referrer` policy. CORS
is allowlist-only via `server.cors_allowed_origins`; with the default
empty list, no CORS headers are sent at all (same-origin only).
