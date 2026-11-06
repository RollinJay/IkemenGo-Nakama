#!/bin/sh
# Runs Caddy in this terminal (Ctrl+C stops it) for the public name that
# set-public-host.sh wrote. The <id>-caddy service runs the same.
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"

if [ ! -r "$HERE/caddy/public-host.env" ]; then
	echo "no public name yet: run ./set-public-host.sh <name> first" >&2
	exit 2
fi
PUBLIC_HOST=$(sed -n 's/^PUBLIC_HOST=//p' "$HERE/caddy/public-host.env" | tail -n 1)
# The Caddyfile keeps its certificates under this folder.
SERVER_KIT=$HERE
export PUBLIC_HOST SERVER_KIT
if [ -z "$CADDY" ] && [ -x "$HERE/programs/caddy/caddy" ]; then
	CADDY=$HERE/programs/caddy/caddy
fi
if [ -z "$CADDY" ]; then
	CADDY=$(command -v caddy) || {
		echo "caddy not found: put it in programs/caddy, or set CADDY= in settings.env" >&2
		exit 2
	}
fi
exec "$CADDY" run --config "$HERE/caddy/Caddyfile" --adapter caddyfile
