#!/bin/sh
# Hopper container entrypoint.
#
# Started as root (the image default), it makes /config and /downloads
# usable for PUID:PGID (default 1000:1000 — the first regular user on most
# Linux hosts, i.e. your files on the host stay yours) and then runs
# hopperd as that user, without root privileges. This replaces running
# docker/setup.sh with sudo before the first start.
#
# Started as a non-root user (docker run --user / compose user:), it does
# nothing but exec — the caller is then responsible for permissions.
set -eu

if [ "$(id -u)" != "0" ]; then
	exec "$@"
fi

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
case "$PUID$PGID" in
*[!0-9]*) echo "entrypoint: PUID/PGID must be numeric (got $PUID:$PGID)" >&2; exit 1 ;;
esac
if [ "$PUID" = "0" ]; then
	echo "entrypoint: refusing to run hopperd as root (PUID=0)" >&2
	exit 1
fi

mkdir -p /config /downloads

# /config belongs to Hopper alone (database, secrets, settings): anything
# in it not owned by PUID:PGID — a fresh root-owned bind mount, or a file
# written by `docker compose exec hopperd hopperd -set-admin-password`,
# which runs as root — is handed over.
# chown can be refused by the filesystem (Docker Desktop bind mounts on
# Windows/macOS, NFS with root_squash) — warn instead of failing to start.
find /config \( ! -user "$PUID" -o ! -group "$PGID" \) -exec chown "$PUID:$PGID" {} + 2>/dev/null ||
	echo "entrypoint: warning: could not change ownership of everything in /config to $PUID:$PGID" >&2

# /downloads may be an existing media library: never chown recursively.
# Only the directory itself, and only if it's root-owned (i.e. Docker just
# created it for the bind mount) — otherwise leave it alone and warn.
if ! setpriv --reuid="$PUID" --regid="$PGID" --clear-groups test -w /downloads; then
	if [ "$(stat -c %u /downloads)" = "0" ]; then
		chown "$PUID:$PGID" /downloads 2>/dev/null ||
			echo "entrypoint: warning: could not chown /downloads to $PUID:$PGID" >&2
	else
		echo "entrypoint: warning: /downloads is not writable for $PUID:$PGID (owner $(stat -c %u:%g /downloads)); set PUID/PGID to its owner or fix permissions on the host" >&2
	fi
fi

exec setpriv --reuid="$PUID" --regid="$PGID" --clear-groups -- "$@"
