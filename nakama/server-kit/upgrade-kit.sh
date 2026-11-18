#!/bin/sh
# Turns a folder set up for a single server (SERVER_ID, GAME_DIR, GAME, BUILD
# and REGION in settings.env, caddy/public-host.env, host-profile.json) into
# the layout for several servers:
#   - servers/<SERVER_ID>.env gets the server's settings and public name,
#   - caddy/sites/<SERVER_ID>.caddy its Caddy site,
#   - host-profiles/<SERVER_ID>.json this PC's profile,
#   - settings.env keeps TITLE, MATCHMAKER and CADDY, gets KIT_ID=<SERVER_ID>,
#     and the moved lines become comments (settings.env.before-upgrade keeps
#     the file as it was),
#   - an installed <SERVER_ID>-caddy service waits for caddy/sites/ instead
#     of public-host.env; the services keep their names (<SERVER_ID>-server,
#     -caddy and -duckdns), whether they start with the PC, and run on.
# Nothing else changes: the server, its database, keys and certificates stay.
# The scripts and the window run it by themselves; running it again does
# nothing.
#
#   ./upgrade-kit.sh
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
SERVER_KIT_UPGRADING=1
export SERVER_KIT_UPGRADING
. "$HERE/settings.sh"

# One upgrade at a time: the services and the window may all start it at
# once. The others wait, then find nothing left to do.
if command -v flock >/dev/null 2>&1 && [ -f "$HERE/settings.env" ]; then
	exec 9<"$HERE/settings.env"
	flock 9
fi

id=$(setting SERVER_ID)
if [ -z "$id" ]; then
	echo "Nothing to upgrade: settings.env names no SERVER_ID."
	exit 0
fi
if ! valid_id "$id"; then
	echo "settings.env: SERVER_ID '$id' is not a server id (lowercase letters, digits and dashes)" >&2
	exit 2
fi
echo "Upgrading this folder to several servers; the server $id keeps its settings."
port=$(server_port "$id") || port=7350
# The file as it was, before any change.
[ -f "$HERE/settings.env.before-upgrade" ] || cp -p "$HERE/settings.env" "$HERE/settings.env.before-upgrade"

# The public name: public-host.env's (PUBLIC_HOST, or a <NAME>_PUBLIC_HOST
# of older copies), else settings.env's.
host=
for file in "$HERE/caddy/public-host.env" "$HERE/settings.env"; do
	if [ -z "$host" ] && [ -r "$file" ]; then
		host=$(sed -nE 's/^(PUBLIC_HOST|[A-Za-z0-9_]*_PUBLIC_HOST)=//p' "$file" | tail -n 1 | tr -d '\r')
	fi
done

# 1. The server's settings.
if [ ! -f "$HERE/servers/$id.env" ]; then
	mkdir -p "$HERE/servers"
	{
		echo "# The server $id (from settings.env, by upgrade-kit.sh). The window's"
		echo "# Public name page and set-public-host.sh change it."
		for key in TITLE GAME_DIR GAME BUILD REGION; do
			printf '%s=%s\n' "$key" "$(setting "$key")"
		done
		printf 'PUBLIC_HOST=%s\n' "$host"
	} > "$HERE/servers/$id.env.tmp.$$"
	mv -f "$HERE/servers/$id.env.tmp.$$" "$HERE/servers/$id.env"
	echo "Wrote servers/$id.env"
fi

# 2. Its Caddy site, in place of public-host.env.
if [ -n "$host" ] && [ ! -f "$HERE/caddy/sites/$id.caddy" ]; then
	mkdir -p "$HERE/caddy/sites"
	printf '%s {\n\timport nakama\n\treverse_proxy 127.0.0.1:%s\n}\n' "$host" "$port" > "$HERE/caddy/sites/$id.caddy.tmp.$$"
	mv -f "$HERE/caddy/sites/$id.caddy.tmp.$$" "$HERE/caddy/sites/$id.caddy"
	echo "Wrote caddy/sites/$id.caddy ($host)"
fi
if [ -f "$HERE/caddy/public-host.env" ] && { [ -z "$host" ] || [ -f "$HERE/caddy/sites/$id.caddy" ]; }; then
	rm -f "$HERE/caddy/public-host.env"
fi

# 3. This PC's profile.
if [ -f "$HERE/host-profile.json" ]; then
	mkdir -p "$HERE/host-profiles"
	chmod 700 "$HERE/host-profiles"
	if [ ! -f "$HERE/host-profiles/$id.json" ]; then
		mv "$HERE/host-profile.json" "$HERE/host-profiles/$id.json"
		chmod 600 "$HERE/host-profiles/$id.json"
		echo "Moved host-profile.json to host-profiles/$id.json"
	else
		rm -f "$HERE/host-profile.json"
	fi
fi

# 4. The shared services keep the server's name.
[ -n "$(setting KIT_ID)" ] || set_env_value "$HERE/settings.env" KIT_ID "$id"

# 5. Installed services: the Caddy one waited for public-host.env, and waits
# for caddy/sites/ now. Only that line changes: the file stays where it is,
# enabled or not as it was.
unit=$UNITS/$id-caddy.service
if [ -f "$unit" ] && ! unit_elsewhere "$id-caddy.service" &&
	grep -q '^ConditionPathExists=.*/caddy/public-host.env' "$unit"; then
	sed "s|^ConditionPathExists=.*/caddy/public-host.env\$|ConditionPathExistsGlob=$HERE/caddy/sites/*.caddy|" "$unit" > "$unit.tmp.$$"
	cat "$unit.tmp.$$" > "$unit"
	rm -f "$unit.tmp.$$"
	systemctl --user daemon-reload
	echo "The $id-caddy service waits for caddy/sites/ now."
fi

# 6. Last, as it ends the upgrade: settings.env without the moved lines.
awk -v id="$id" '
	/^(SERVER_ID|GAME_DIR|GAME|BUILD|REGION|[A-Za-z0-9_]*PUBLIC_HOST)=/ {
		print "# now in servers/" id ".env: " $0
		next
	}
	{ print }
' "$HERE/settings.env" > "$HERE/settings.env.tmp.$$"
cat "$HERE/settings.env.tmp.$$" > "$HERE/settings.env"
rm -f "$HERE/settings.env.tmp.$$"
echo "Done: settings.env is the whole folder's now (KIT_ID=$id), servers/$id.env the server's."
