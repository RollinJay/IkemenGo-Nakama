#!/bin/sh
# Starts the game signed in to one of the servers on this PC directly
# (127.0.0.1, host-profiles/<id>.json), instead of through the public name the
# shared folder uses. Many home routers cannot reach their own public address
# from inside the network, and the certificate is for the public name anyway.
# Everything else (your account, matches, lobbies) is the same as for players.
#
#   ./play-on-this-pc.sh [id] [game folder]
# The id may be left out with a single server; the game folder is the
# server's GAME_DIR unless given.
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"
if [ -n "$1" ] && valid_id "$1" && { [ -f "$HERE/servers/$1.env" ] || [ ! -d "$1" ]; }; then
	need_server_id "$1"
	id=$1
	shift
else
	id=$(pick_server) || exit
fi
load_server "$id"
GAME_DIR=${1:-$S_GAME_DIR}
if [ -z "$GAME_DIR" ]; then
	echo "no game folder: set GAME_DIR= in servers/$id.env, or give it after the id" >&2
	exit 2
fi
PROFILE=$HERE/host-profiles/$id.json
if [ ! -r "$PROFILE" ]; then
	echo "no $PROFILE yet: run ./set-public-host.sh $id <public name> first" >&2
	exit 1
fi

# Same pools as the players: the builds must match.
mine=$(python3 -I -c 'import json,sys; p=json.load(open(sys.argv[1])); print(next(s["build"] for s in p["servers"] if s["id"]==p["default"]))' "$PROFILE") || exit 1
theirs=$(python3 -I -c 'import json,sys; p=json.load(open(sys.argv[1])); print(next(s["build"] for s in p["servers"] if s["id"]==sys.argv[2]))' "$GAME_DIR/data/online/servers.json" "$id") || {
	echo "the game folder's data/online/servers.json has no profile $id: run ./set-public-host.sh $id <public name>" >&2
	exit 1
}
if [ "$mine" != "$theirs" ]; then
	echo "host-profiles/$id.json is for build $mine, the game folder for $theirs: run ./set-public-host.sh again" >&2
	exit 1
fi

cd "$GAME_DIR" || exit 1
exec ./Ikemen_GO_Linux -nakama-config "$PROFILE"
