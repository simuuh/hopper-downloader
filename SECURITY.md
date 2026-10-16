# Security policy

Hopper stores hoster credentials and exposes an API that can make the
host download arbitrary URLs, so security reports are taken seriously.

## Reporting a vulnerability

**Please do not open a public issue.** Report privately via GitHub:
**Security → Report a vulnerability** on this repository
(<https://github.com/simuuh/hopper-downloader/security/advisories/new>).

Include what you can: affected version/commit, configuration, steps to
reproduce, impact. You'll get an acknowledgement within a few days; a
fix and advisory follow once it's confirmed.

## Supported versions

Only the latest release receives security fixes.

## Scope

In scope, for example: authentication/JWT bypass, credential exposure
(logs, API, storage), path traversal when writing downloads or
extracting archives, Click'n'Load trust bypass (`trusted_cidrs`),
XSS/CSRF in the Web UI, the browser extension leaking the API token.

Out of scope: an authenticated user making hopperd fetch internal URLs
(by design — see [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#security-model)),
denial of service by an authenticated user, and issues in third-party
hoster services.
