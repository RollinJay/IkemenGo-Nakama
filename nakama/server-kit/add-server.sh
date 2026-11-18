#!/bin/sh
# Adds a server to this folder: one more game, or another build, region or
# community of the same game. Each server has its own Nakama, database,
# accounts and ports, its own public name, and its own service
# (<id>-server); Caddy and the DuckDNS updater serve them all.
#
#   ./add-server.sh <id> [--title T] [--game-dir DIR] [--game G] [--build B]
#                        [--region R] [--port N] [--database-port N]
#
# <id> is lowercase letters, digits and dashes, such as mygame or mygame-eu;
# it names the server's service (<id>-server) and its folder in config/. The
# API port is the first of 7350, 7360, 7370... no server here uses, unless
# --port gives one; the console listens on the next port. A server that
# "./server.sh add" made already joins as it is (its ports stay).
#
# The settings go into servers/<id>.env; set-public-host.sh uses them. The
# first server's id also becomes KIT_ID in settings.env, the name of the
# shared services.
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"

usage() {
	echo "usage: $0 <id> [--title T] [--game-dir DIR] [--game G] [--build B] [--region R] [--port N] [--database-port N]" >&2
	exit 2
}
id=$1
[ -n "$id" ] || usage
shift
title= game_dir= game= build= region= port= database_port=
while [ $# -gt 0 ]; do
	[ $# -ge 2 ] || usage
	case $1 in
	--title) title=$2 ;;
	--game-dir) game_dir=$2 ;;
	--game) game=$2 ;;
	--build) build=$2 ;;
	--region) region=$2 ;;
	--port) port=$2 ;;
	--database-port) database_port=$2 ;;
	*) usage ;;
	esac
	shift 2
done

if ! valid_id "$id"; then
	echo "'$id' is not a server id: lowercase letters, digits and dashes, starting with a letter or digit" >&2
	exit 2
fi
if [ -f "$HERE/servers/$id.env" ]; then
	echo "There is a server '$id' here already (servers/$id.env)" >&2
	exit 2
fi
if unit_elsewhere "$id-server.service"; then
	echo "Another folder's service is named $id-server already ($UNITS/$id-server.service): choose another id" >&2
	exit 2
fi
for value in "$game" "$build" "$region"; do
	if [ -n "$value" ] && ! printf '%s' "$value" | grep -Eq '^[A-Za-z0-9._-]+$'; then
		echo "'$value' is not a game id, build or region (letters, digits, dots, dashes, underscores)" >&2
		exit 2
	fi
done
for value in "$port" "$database_port"; do
	if [ -n "$value" ] && ! printf '%s' "$value" | grep -Eq '^[1-9][0-9]{0,4}$'; then
		echo "'$value' is not a port" >&2
		exit 2
	fi
done
case $title$game_dir in
*"
"*)
	echo "The title and the game folder are one line each" >&2
	exit 2
	;;
esac
if [ -n "$game_dir" ]; then
	if [ ! -d "$game_dir" ]; then
		echo "No folder '$game_dir'" >&2
		exit 2
	fi
	game_dir=$(CDPATH= cd -- "$game_dir" && pwd)
fi
[ -n "$title" ] || title=$id

if old_port=$(server_port "$id"); then
	# Made with "./server.sh add" before: it keeps its settings and ports.
	if [ -n "$port" ] && [ "$port" != "$old_port" ]; then
		echo "The server program has a server '$id' already, on port $old_port: change its port with ./server.sh set $id --port N" >&2
		exit 2
	fi
	port=$old_port
	echo "The server program has a server '$id' already: adding it as it is."
else
	# The API port, and the ports Nakama takes around it (the one before it;
	# the console's, and 3 before that), clear of every other server's here
	# and of the ports programs already listen on (another folder's servers).
	# The database's port too: the server program counts from 54331 in this
	# folder only, as another folder's does.
	ports=$(python3 -I - "$HERE/config/servers.json" "$port" "$database_port" <<'EOF'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        servers = json.load(f).get("servers", [])
except (OSError, ValueError):
    servers = []
taken = set()
for s in servers:
    p = s.get("port") or 7350
    c = s.get("console_port") or p + 1
    taken |= {p, p - 1, c, c - 3}
    if s.get("database_port"):
        taken.add(s["database_port"])
listening = set()
for table in ("/proc/net/tcp", "/proc/net/tcp6"):
    try:
        with open(table) as f:
            for line in f.readlines()[1:]:
                fields = line.split()
                if fields[3] == "0A":  # LISTEN
                    listening.add(int(fields[1].rsplit(":", 1)[1], 16))
    except (OSError, IndexError, ValueError):
        pass
def nakama_ports(p):
    return {p, p - 1, p + 1, p - 2}
wanted = sys.argv[2]
if wanted:
    p = int(wanted)
    if not 1024 < p < 65535 or taken & nakama_ports(p):
        sys.exit(f"port {p}, or one Nakama takes next to it ({p - 2} to {p + 1}), is another server's here")
    if listening & nakama_ports(p):
        sys.exit(f"a program listens on port {p}, or next to it ({p - 2} to {p + 1}), already")
else:
    p = next(p for p in range(7350, 65000, 10) if not (taken | listening) & nakama_ports(p))
taken |= nakama_ports(p)
wanted = sys.argv[3]
if wanted:
    d = int(wanted)
    if not 1024 < d < 65536 or d in taken:
        sys.exit(f"database port {d} is another server's here, or this one's Nakama's")
    if d in listening:
        sys.exit(f"a program listens on port {d} already")
else:
    d = next(d for d in range(54331, 65000) if d not in taken | listening)
print(p, d)
EOF
)
	port=${ports% *} database_port=${ports#* }
	set -- --kinds ikemen --title "$title" --bind 127.0.0.1 --public-port 443 --client-tls yes \
		--port "$port" --console-port "$((port + 1))" --database-port "$database_port"
	"$HERE/server.sh" add "$id" "$@" >/dev/null
fi

mkdir -p "$HERE/servers"
cat > "$HERE/servers/$id.env.tmp" <<EOF
# The server $id (add-server.sh). The window's Public name page and
# set-public-host.sh change it. One KEY=value per line, without quotes.
TITLE=$title
GAME_DIR=$game_dir
GAME=${game:-$id}
BUILD=$build
REGION=${region:-us-west}
PUBLIC_HOST=
EOF
mv -f "$HERE/servers/$id.env.tmp" "$HERE/servers/$id.env"

# The shared services keep the first server's name, whatever is added or
# removed later.
if [ -z "$(setting KIT_ID)" ]; then
	set_env_value "$HERE/settings.env" KIT_ID "$(kit_id)"
	[ -n "$(setting TITLE)" ] || set_env_value "$HERE/settings.env" TITLE "$title"
fi

echo "Added the server $id: API port $port (127.0.0.1), console port $((port + 1)); settings in servers/$id.env"
if services_installed; then
	"$HERE/install-services.sh" --server "$id"
else
	echo "Install the services when ready: ./install-services.sh"
fi
if [ -z "$build" ]; then
	echo "Next: set BUILD in servers/$id.env, then ./set-public-host.sh $id <public name>"
else
	echo "Next: ./set-public-host.sh $id <public name>"
fi
