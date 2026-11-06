#!/bin/sh
# Sets the name players' games use to reach this server, such as
# mygame.duckdns.org, everywhere that needs it:
#   - the game folder's data/online/servers.json, which your players get,
#   - host-profile.json, the same profile pointed at 127.0.0.1 for this PC's
#     own game (play-on-this-pc.sh),
#   - the server's public address (what "./server.sh show <id>" prints),
#   - Caddy's site (caddy/public-host.env), whose certificate is for that name,
# with the game id, build and region from settings.env. Run it again after
# changing them.
#
#   ./set-public-host.sh <name> [game folder]
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"
need_server_id
if [ -z "$BUILD" ]; then
	echo "settings.env: set BUILD, the build your players' copies are paired by (nakama/hosting.md, step 8)" >&2
	exit 2
fi
for value in "$GAME" "$BUILD" "$REGION"; do
	if ! printf '%s' "$value" | grep -Eq '^[A-Za-z0-9._-]+$'; then
		echo "settings.env: '$value' is not a game id, build or region (letters, digits, dots, dashes, underscores)" >&2
		exit 2
	fi
done

HOST=$1
GAME_DIR=${2:-$GAME_DIR}

if [ -z "$HOST" ]; then
	echo "usage: $0 <public name, e.g. mygame.duckdns.org> [game folder]" >&2
	exit 2
fi
# A full name, not an IP address: the certificate is issued for a name.
if ! printf '%s' "$HOST" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$' ||
	printf '%s' "$HOST" | grep -Eq '^[0-9.]+$'; then
	echo "'$HOST' is not a full host name (lowercase letters, digits, dots and dashes)" >&2
	exit 2
fi
if [ -z "$GAME_DIR" ] || [ ! -d "$GAME_DIR/data/online" ]; then
	echo "no data/online folder in '$GAME_DIR': set GAME_DIR= in settings.env, or give the game folder as the second argument" >&2
	exit 2
fi

# The game folder first: it is the step most likely to fail, and nothing
# else changes until it has worked.
"$HERE/server.sh" ikemen-profile "$SERVER_ID" "$GAME_DIR/data/online/servers.json" \
	--game "$GAME" --build "$BUILD" --region "$REGION" --host "$HOST" >/dev/null
python3 -I - "$GAME_DIR/data/online/servers.json" "$HERE/host-profile.json" "$SERVER_ID" <<'EOF'
import json, os, sys
src, dst, server_id = sys.argv[1], sys.argv[2], sys.argv[3]
with open(src, encoding="utf-8") as f:
    profiles = json.load(f)
server = next(s for s in profiles["servers"] if s["id"] == profiles["default"])
server = dict(server, id=server_id + "-this-pc", name=server.get("name", "") + " (this PC)",
              host="127.0.0.1", port=7350, ssl=False)
tmp = dst + ".tmp"
with os.fdopen(os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w",
               encoding="utf-8") as f:
    json.dump({"version": 1, "default": server["id"], "servers": [server]}, f, indent=2)
    f.write("\n")
os.replace(tmp, dst)
EOF
"$HERE/server.sh" set "$SERVER_ID" --public-host "$HOST" >/dev/null
mkdir -p "$HERE/caddy"
printf 'PUBLIC_HOST=%s\n' "$HOST" > "$HERE/caddy/public-host.env"

echo "Public name: $HOST"
echo "Game profile: $GAME_DIR/data/online/servers.json (game $GAME, build $BUILD, region $REGION, port 443, TLS)"
echo "This PC's profile: $HERE/host-profile.json (127.0.0.1:7350, build $BUILD)"
if systemctl --user is-enabled --quiet "$SERVER_ID-caddy" 2>/dev/null; then
	# Starts it too: without a name, the service waited (see its unit).
	systemctl --user restart "$SERVER_ID-caddy"
	echo "Started the $SERVER_ID-caddy service for the new name."
else
	echo "Start Caddy for this name: ./run-caddy.sh, or install the services (./install-services.sh)."
fi
