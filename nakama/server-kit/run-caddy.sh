#!/bin/sh
# Runs Caddy in this terminal (Ctrl+C stops it) for every public name that
# set-public-host.sh wrote (caddy/sites/<id>.caddy). The <KIT_ID>-caddy
# service runs the same.
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"

found=
for site in "$HERE"/caddy/sites/*.caddy; do
	[ -f "$site" ] && found=1
done
if [ -z "$found" ]; then
	echo "no public name yet: run ./set-public-host.sh <id> <name> first" >&2
	exit 2
fi
# The Caddyfile keeps its certificates under this folder.
SERVER_KIT=$HERE
export SERVER_KIT
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
