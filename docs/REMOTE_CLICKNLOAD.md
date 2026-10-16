# Click'n'Load with hopperd on a NAS

Click'n'Load buttons on hoster sites always send links to
`127.0.0.1:9666` — the machine the browser runs on. When hopperd runs on
a NAS or server, something on your PC has to accept those requests and
pass them on. Two options:

| | Browser extension | hopperctl |
|---|---|---|
| Install | add-on in Chrome/Edge/Firefox | a binary + OS service |
| Click'n'Load | yes | yes |
| `.dlc` / `.txt` downloads imported automatically | yes (needs an API token) | yes (folder watch) |
| Certificate step | none | once per browser |
| Works in Brave | yes | often blocked by Shields |

The extension is the recommended way: nothing to install on the PC
besides the add-on, and no certificate to trust.

```
browser ──CnL──▶ extension or hopperctl (on your PC) ──▶ hopperd :9666 (NAS)
                                                     └─▶ hopperd :9667 (.dlc/.txt import)
```

## 1. Make hopperd's Click'n'Load port reachable

In the `.env` next to `docker-compose.yml`, set `CNL_HOST=0.0.0.0` and
run `docker compose up -d`. The Compose file already trusts the
private address ranges (10/8, 172.16/12, 192.168/16) inside the
container, so nothing else needs changing for a normal home network.

Running hopperd from source instead: set `clicknload.host: 0.0.0.0` and
widen `clicknload.trusted_cidrs` in `hopper.yaml`.

## 2a. Browser extension

**Chrome / Edge**
1. Download `hopper-extension.zip` from the
   [latest release](https://github.com/simuuh/hopper-downloader/releases) and unzip it.
2. `chrome://extensions` → enable Developer mode → "Load unpacked" → the
   unzipped folder.
3. Open the extension's options and enter your hopperd address.

**Firefox (128 or later)**
Install `hopper-extension.xpi` from the release (signed by Mozilla, when
present), or load `manifest.json` from the zip temporarily via
`about:debugging` → This Firefox → "Load Temporary Add-on". Firefox
before 128 can't inject the page script the extension relies on.

Automatic `.dlc` / `.txt` import also needs an API token pasted into the
options page (there is no login in the extension); see the options page
for how to get one and why a longer `server.token_ttl` helps.

The extension intercepts Click'n'Load inside the page before any network
request is made, which is why it also works in Brave and needs no
certificate.

## 2b. hopperctl

```sh
hopperctl service install --target http://NAS-IP:9666
hopperctl status
```

This writes `hopperctl.yaml`, registers a service (systemd --user,
launchd, or a real Windows Service) and starts a local Click'n'Load relay
plus an optional `.dlc` / `.txt` folder watcher that imports into hopperd
(`hopperctl login` once for that — it uses the authenticated API).

`hopperctl.yaml` has two targets, because Click'n'Load (port 9666, no
auth beyond `trusted_cidrs`) and the REST API (port 9667, JWT) are
different listeners:

```yaml
target: "http://NAS-IP:9666"       # Click'n'Load relay target
api_target: "http://NAS-IP:9667"   # .dlc/.txt import target (optional)
```

`api_target` is derived from `target` (port 9667) when left blank;
`--api-target` on `service install` sets it.

**Trust the certificate once per browser.** The relay serves
`https://127.0.0.1:9666` — hoster pages are HTTPS and browsers block
their requests to plain HTTP. Open `https://127.0.0.1:9666/flash/` and
accept the warning (Chrome/Edge: Advanced → Proceed; Firefox: Advanced →
Accept the Risk). Browsers don't trust a self-signed certificate just
because it's for localhost. The certificate is reused across restarts
(valid 10 years), so this is a one-time step; `hopperctl status` shows
its path and expiry.

### Windows

`service install` registers a real Windows Service ("HopperCtl" in
`services.msc`) that starts at boot without a console window. It runs as
LocalSystem, so it reads `%ProgramData%\hopper\hopperctl.yaml`;
`service install` writes that and your own
`%APPDATA%\hopper\hopperctl.yaml` (used when you run hopperctl by hand).
They are not kept in sync after install. Logs go to
`%ProgramData%\hopper\hopperctl.log`. The service has its own
certificate, so the browser step above applies to it separately.

```sh
sc query HopperCtl
hopperctl service uninstall   # removes the service and the ProgramData config
```

## Limitations

- **Brave / hardened Firefox** can block a page's request to
  `127.0.0.1` even with a trusted certificate. Use the extension, allow
  the site in Shields, or import the `.dlc` instead.
- **Phones and tablets** can't use Click'n'Load at all — it always
  targets the device the browser runs on. Share the link to Hopper
  instead (see [USAGE.md](USAGE.md#from-a-phone)).

More in [TROUBLESHOOTING.md](TROUBLESHOOTING.md) and
[COMPATIBILITY.md](COMPATIBILITY.md).
