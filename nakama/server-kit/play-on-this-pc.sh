#!/bin/sh
# Starts the game signed in to the server on this PC directly
# (127.0.0.1:7350, host-profile.json), instead of through the public name the
# shared folder uses. Many home routers cannot reach their own public address
# from inside the network, and the certificate is for the public name anyway.
# Everything else (your account, matches, lobbies) is the same as for players.
#
#   ./play-on-this-pc.sh [game folder]
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"
GAME_DIR=${1:-$GAME_DIR}
if [ -z "$GAME_DIR" ]; then
	echo "no game folder: set GAME_DIR= in settings.env, or give it as the argument" >&2
	exit 2
fi

# Same pools as the players: the builds must match.
mine=$(python3 -I -c 'import json,sys; p=json.load(open(sys.argv[1])); print(next(s["build"] for s in p["servers"] if s["id"]==p["default"]))' "$HERE/host-profile.json") || exit 1
theirs=$(python3 -I -c 'import json,sys; p=json.load(open(sys.argv[1])); print(next(s["build"] for s in p["servers"] if s["id"]==p["default"]))' "$GAME_DIR/data/online/servers.json") || exit 1
if [ "$mine" != "$theirs" ]; then
	echo "host-profile.json is for build $mine, the game folder for $theirs: run ./set-public-host.sh again" >&2
	exit 1
fi

cd "$GAME_DIR" || exit 1
exec ./Ikemen_GO_Linux -nakama-config "$HERE/host-profile.json"
