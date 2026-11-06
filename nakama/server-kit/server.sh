#!/bin/sh
# Runs Nakama Matchmaker's server program with this folder's configuration
# (config/), for example:
#   ./server.sh add mygame --kinds ikemen ...   create the server (README.md)
#   ./server.sh show mygame                      what players enter
#   ./server.sh run mygame                       run it in this terminal
# The <id>-server service runs "run <id>" through it.
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"

if [ -z "$MATCHMAKER" ]; then
	for program in "$HERE/programs/matchmaker/bin/nakama-matchmaker-server" \
		"$HERE/programs/matchmaker/nakama-matchmaker-server/bin/nakama-matchmaker-server"; do
		if [ -x "$program" ]; then
			MATCHMAKER=$program
			break
		fi
	done
fi
if [ -z "$MATCHMAKER" ]; then
	MATCHMAKER=$(command -v nakama-matchmaker-server) || {
		echo "nakama-matchmaker-server not found: unpack it in programs/matchmaker, or set MATCHMAKER= in settings.env" >&2
		exit 2
	}
fi
# PostgreSQL's programs unpacked from Debian's packages into programs/postgresql
# need the libpq next to them; a PostgreSQL installed the usual way does not.
for lib in "$HERE"/programs/postgresql/usr/lib/*-linux-gnu; do
	if [ -d "$lib" ]; then
		LD_LIBRARY_PATH="$lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
		export LD_LIBRARY_PATH
	fi
done
# Nakama sends anonymous usage statistics to Heroic Labs unless this is 0.
NAKAMA_TELEMETRY=${NAKAMA_TELEMETRY:-0}
export NAKAMA_TELEMETRY
exec "$MATCHMAKER" --config "$HERE/config" "$@"
