# Installation and configuration

- [Docker Compose](#docker-compose)
- [From source](#from-source)
- [Configuration](#configuration)
- [Security model](#security-model)

## Docker Compose

No git checkout, no secrets to prepare — two files, one of them yours to
edit:

```sh
mkdir hopper && cd hopper
curl -fsSLO https://raw.githubusercontent.com/simuuh/hopper-downloader/main/docker-compose.yml
curl -fsSLo .env https://raw.githubusercontent.com/simuuh/hopper-downloader/main/.env.example
nano .env                          # PUID/PGID, downloads folder, ports
docker compose up -d
docker compose logs hopperd        # first start: prints the admin password
```

Open `http://<host>:9667` and sign in as `admin` with the printed
password. Change it any time:

```sh
docker compose exec -it hopperd hopperd -set-admin-password
docker compose restart hopperd
```

**Update:** `docker compose pull && docker compose up -d`. All settings
live in `.env`; `docker-compose.yml` stays untouched, so it can simply be
replaced by a newer one. Needs Docker Compose 2.24 or newer.

### .env

[`.env.example`](../.env.example) documents every line; all are
optional (without a `.env` the defaults apply).

| Variable | Default | Meaning |
| --- | --- | --- |
| `PUID` / `PGID` | `1000` | Owner of your downloads folder (`id <user>`; Synology often 1026/100, Unraid 99/100) |
| `CONFIG_DIR` | `./config` | Database, `secrets.json`, optional `hopper.yaml` |
| `DOWNLOADS_DIR` | `./downloads` | Host folder for downloads, e.g. an existing media library |
| `WEB_PORT` | `9667` | Host port of the Web UI / API |
| `CNL_HOST` | `127.0.0.1` | `0.0.0.0` makes Click'n'Load reachable from your LAN |
| `CNL_PORT` | `9666` | Host port of Click'n'Load |
| `IMAGE_TAG` | `latest` | Pin a release, e.g. `v0.6.0` |
| `HOPPER_*` | – | Any hopperd setting, e.g. `HOPPER_NOTIFY_NTFY_URL` (see [USAGE.md](USAGE.md#notifications)) or `HOPPER_LOG_LEVEL` |

The rule behind the names: **everything starting with `HOPPER_` is passed
to hopperd; the Docker settings above never start with `HOPPER_`**, so
the two can't be confused (a test enforces it). The full list of
`HOPPER_*` settings is in [hopper.example.yaml](../hopper.example.yaml) and
[Configuration](#configuration) below. Most download settings (parallel
downloads, bandwidth, auto-extract, …) are simply set in the Web UI.

A bcrypt hash in `.env` (`HOPPER_ADMIN_PASSWORD_HASH`) must be in
**single quotes** — it contains `$`, which Compose would otherwise
interpolate.

**What's in `./config`:** the database and `secrets.json` (generated on
first start; it holds the key that decrypts your stored hoster accounts).
Back up the whole folder.

The container makes `./config` and the downloads folder writable for
`PUID:PGID` and then runs hopperd as that user — no `sudo` or `chown`
needed. An existing downloads folder is never changed recursively; if it
isn't writable for `PUID:PGID`, the log says so.

**arm64 (Raspberry Pi, ARM servers):** images are multi-arch
(`linux/amd64`, `linux/arm64`); binaries ship for Linux, macOS and Windows
on amd64 and arm64.

**Building the image yourself** (development, unreleased changes):
`git clone https://github.com/simuuh/hopper-downloader && cd hopper/docker &&
docker compose up --build -d` — same `.env` settings, in `docker/.env`.

## From source

Requires Go 1.26+ and Node 24+.

```sh
make web    # builds web/dist (embedded into hopperd via go:embed)
make build  # go build ./...

go run ./cmd/hopperd     # no config file needed; secrets are generated on
                         # first start, the admin password is printed once
```

Without a `hopper.yaml`, hopperd uses its defaults (downloads in
`/downloads`, database in `/config`) — set `HOPPER_DOWNLOADS_PATH` and
`HOPPER_DATABASE_PATH`, or copy [hopper.example.yaml](../hopper.example.yaml)
to `hopper.yaml` and pass `-config hopper.yaml`. Explicitly configured
secrets (`HOPPER_JWT_SECRET`, `HOPPER_ADMIN_PASSWORD_HASH` from
`hopperctl hash-password`, `HOPPER_ENCRYPTION_KEY`) always take
precedence over generated ones. Auto-extraction of RAR archives needs an
`unrar` binary (the Docker image includes it).

## Configuration

Every option is documented inline in
[hopper.example.yaml](../hopper.example.yaml). Config is YAML with
`${VAR}` expansion, plus direct `HOPPER_*` environment variable overrides
(env wins over file); [internal/config](../internal/config/config.go) has
the authoritative list. Most download settings (folder, parallelism,
bandwidth, retries, extraction, log level) can be changed live in the Web
UI under Settings, without a restart.

## Security model

- JWT auth (HS256) on every `/api/v1/*` endpoint; a single admin identity
  (bcrypt hash), no hardcoded secrets.
- Hoster account credentials encrypted at rest with AES-256-GCM.
- Path-traversal protection on every filesystem write derived from a
  hoster- or user-supplied name (`internal/pathsafe`).
- Click'n'Load listener is localhost-only by default; configurable
  trusted CIDRs for remote/LAN setups.
- Strict CORS (same-origin by default), security headers (CSP, HSTS,
  X-Frame-Options, …) on every response, per-client API rate limiting.
- No telemetry, no analytics, no phone-home. Outbound calls go only to
  the URLs you queue, your hosters' APIs, JDownloader's public
  key-exchange service for `.dlc` import (see
  [internal/dlc](../internal/dlc/crypto.go) for why that's unavoidable),
  and the notification targets you configure.

Security issues: see [SECURITY.md](../SECURITY.md).
