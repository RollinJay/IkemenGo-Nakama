#!/bin/sh
# Sets the name players' games use to reach one server, such as
# mygame.duckdns.org, everywhere that needs it:
#   - the game folder's data/online/servers.json, which your players get: the
#     server's profile there (its id is the server's), next to the profiles
#     already there,
#   - host-profiles/<id>.json, the same profile pointed at 127.0.0.1 for this
#     PC's own game (play-on-this-pc.sh),
#   - the server's public address (what "./server.sh show <id>" prints),
#   - Caddy's site for it (caddy/sites/<id>.caddy), whose certificate is for
#     that name,
# with the title, game folder, game id, build and region from
# servers/<id>.env, where it saves the name too (PUBLIC_HOST). Run it again
# after changing them. Every server needs a name of its own.
#
#   ./set-public-host.sh <id> <name> [game folder]
#   ./set-public-host.sh <name> [game folder]       with a single server
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"

# Names have dots, ids have none.
case $1 in
*.*)
	id=$(pick_server) || exit
	HOST=$1
	GAME_DIR=$2
	;;
*)
	if [ -z "$1" ] || [ -z "$2" ]; then
		echo "usage: $0 <id> <public name, e.g. mygame.duckdns.org> [game folder]" >&2
		exit 2
	fi
	need_server_id "$1"
	id=$1
	HOST=$2
	GAME_DIR=$3
	;;
esac
load_server "$id"
GAME_DIR=${GAME_DIR:-$S_GAME_DIR}
if [ -z "$S_BUILD" ]; then
	echo "servers/$id.env: set BUILD, the build your players' copies are paired by (nakama/hosting.md, step 8)" >&2
	exit 2
fi
for value in "$S_GAME" "$S_BUILD" "$S_REGION"; do
	if ! printf '%s' "$value" | grep -Eq '^[A-Za-z0-9._-]+$'; then
		echo "servers/$id.env: '$value' is not a game id, build or region (letters, digits, dots, dashes, underscores)" >&2
		exit 2
	fi
done
# A full name, not an IP address: the certificate is issued for a name.
if ! printf '%s' "$HOST" | grep -Eq '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$' ||
	printf '%s' "$HOST" | grep -Eq '^[0-9.]+$'; then
	echo "'$HOST' is not a full host name (lowercase letters, digits, dots and dashes)" >&2
	exit 2
fi
# One name, one site: Caddy cannot send a name to two servers.
for other in $(server_ids); do
	[ "$other" != "$id" ] || continue
	if [ "$(server_setting "$other" PUBLIC_HOST)" = "$HOST" ] ||
		{ [ -f "$HERE/caddy/sites/$other.caddy" ] && head -n 1 "$HERE/caddy/sites/$other.caddy" | grep -qxF "$HOST {"; }; then
		echo "The server $other has the name $HOST already: give each server a name of its own" >&2
		exit 2
	fi
done
if [ -z "$GAME_DIR" ] || [ ! -d "$GAME_DIR/data/online" ]; then
	echo "no data/online folder in '$GAME_DIR': set GAME_DIR= in servers/$id.env, or give the game folder after the name" >&2
	exit 2
fi
if ! port=$(server_port "$id"); then
	echo "The server program has no server '$id' (./server.sh list)" >&2
	exit 2
fi

# The game folder first: it is the step most likely to fail, and nothing
# else changes until it has worked. The server program writes a whole file,
# so it writes a file of its own, which joins the game folder's.
work=$(mktemp -d "$HERE/.set-public-host.XXXXXX")
trap 'rm -rf "$work"' EXIT
"$HERE/server.sh" ikemen-profile "$id" "$work/profile.json" \
	--game "$S_GAME" --build "$S_BUILD" --region "$S_REGION" --host "$HOST" >/dev/null
mkdir -p "$HERE/host-profiles"
chmod 700 "$HERE/host-profiles"
python3 -I - "$work/profile.json" "$GAME_DIR/data/online/servers.json" \
	"$HERE/host-profiles/$id.json" "$id" "$S_TITLE" "$port" <<'EOF'
import json, os, sys
src, game_file, host_file, server_id, title, port = sys.argv[1:]

def write(path, data, mode):
    tmp = path + ".tmp"
    with os.fdopen(os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, mode), "w",
                   encoding="utf-8") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    os.chmod(tmp, mode)
    os.replace(tmp, path)

with open(src, encoding="utf-8") as f:
    made = json.load(f)
server = next(s for s in made["servers"] if s["id"] == server_id)
server["name"] = title

# The game folder's profiles: this server's replaced, the others kept, and
# the default kept unless it names none of them.
try:
    with open(game_file, encoding="utf-8") as f:
        profiles = json.load(f)
    mode = os.stat(game_file).st_mode & 0o777
except FileNotFoundError:
    profiles, mode = {}, 0o644
except ValueError as e:
    sys.exit(f"{game_file} is not valid JSON ({e}): fix or remove it first")
servers = [s for s in profiles.get("servers", []) if s.get("id") != server_id]
at = next((i for i, s in enumerate(profiles.get("servers", [])) if s.get("id") == server_id),
          len(servers))
servers.insert(at, server)
default = profiles.get("default")
if default not in {s.get("id") for s in servers}:
    default = server_id
write(game_file, dict(profiles, version=profiles.get("version") or 1, default=default,
                      servers=servers), mode)

mine = dict(server, id=server_id + "-this-pc", name=title + " (this PC)",
            host="127.0.0.1", port=int(port), ssl=False)
write(host_file, {"version": 1, "default": mine["id"], "servers": [mine]}, 0o600)
EOF
"$HERE/server.sh" set "$id" --public-host "$HOST" --title "$S_TITLE" >/dev/null
mkdir -p "$HERE/caddy/sites"
printf '%s {\n\timport nakama\n\treverse_proxy 127.0.0.1:%s\n}\n' "$HOST" "$port" > "$work/site.caddy"
mv -f "$work/site.caddy" "$HERE/caddy/sites/$id.caddy"
set_server_setting "$id" PUBLIC_HOST "$HOST"

echo "Public name of $id: $HOST"
echo "Game profile: $GAME_DIR/data/online/servers.json, profile $id (game $S_GAME, build $S_BUILD, region $S_REGION, port 443, TLS)"
echo "This PC's profile: $HERE/host-profiles/$id.json (127.0.0.1:$port, build $S_BUILD)"
case $HOST in
*.duckdns.org) echo "ddns/duckdns-update.sh keeps $HOST pointing here (with your token in ddns/duckdns.env)." ;;
esac
if caddy_enabled; then
	# Starts it too: without a name, the service waited (see its unit).
	systemctl --user restart "$KIT_ID-caddy"
	echo "Restarted the $KIT_ID-caddy service for the new name."
else
	echo "Start Caddy for this name: ./run-caddy.sh, or install the services (./install-services.sh)."
fi
