#!/bin/sh
# Removes a server from this folder: stops and removes its service, takes its
# name off Caddy, removes its profile from its game folder's
# data/online/servers.json (another profile there becomes the default if it
# was) and this PC's profile (host-profiles/<id>.json), forgets its settings
# (servers/<id>.env), and has the server program forget it.
#
#   ./remove-server.sh <id> [--delete-data]
#
# Its folder, config/servers/<id> (its database, accounts and logs), stays
# unless --delete-data is given. Players' copies of the game keep the profile
# until they get a new zip.
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"

usage() {
	echo "usage: $0 <id> [--delete-data]" >&2
	exit 2
}
id= delete_data=
for arg in "$@"; do
	case $arg in
	--delete-data) delete_data=--delete-data ;;
	-*) usage ;;
	*) [ -z "$id" ] || usage; id=$arg ;;
	esac
done
[ -n "$id" ] || usage
need_server_id "$id"
load_server "$id"

# The shared services keep their name when their namesake goes.
[ -n "$(setting KIT_ID)" ] || set_env_value "$HERE/settings.env" KIT_ID "$KIT_ID"

if [ -f "$UNITS/$id-server.service" ] && ! unit_elsewhere "$id-server.service"; then
	systemctl --user disable --now "$id-server.service" 2>/dev/null || true
	rm -f "$UNITS/$id-server.service"
	systemctl --user daemon-reload
	echo "Stopped and removed the $id-server service."
fi

if [ -f "$HERE/caddy/sites/$id.caddy" ]; then
	rm -f "$HERE/caddy/sites/$id.caddy"
	if caddy_enabled; then
		left=
		for site in "$HERE"/caddy/sites/*.caddy; do
			[ -f "$site" ] && left=1
		done
		if [ -n "$left" ]; then
			systemctl --user restart "$KIT_ID-caddy"
			echo "Restarted the $KIT_ID-caddy service without $S_PUBLIC_HOST."
		else
			systemctl --user stop "$KIT_ID-caddy"
			echo "Stopped the $KIT_ID-caddy service: no server has a public name now."
		fi
	fi
fi
rm -f "$HERE/host-profiles/$id.json"

if [ -n "$S_GAME_DIR" ] && [ -f "$S_GAME_DIR/data/online/servers.json" ]; then
	python3 -I - "$S_GAME_DIR/data/online/servers.json" "$id" <<'EOF'
import json, os, sys
path, server_id = sys.argv[1:]
try:
    with open(path, encoding="utf-8") as f:
        profiles = json.load(f)
except ValueError as e:
    sys.exit(f"{path} is not valid JSON ({e}): remove the profile {server_id} from it yourself")
servers = profiles.get("servers", [])
left = [s for s in servers if s.get("id") != server_id]
if len(left) == len(servers):
    sys.exit(0)
if not left:
    # The game refuses a file without profiles as it does no file.
    os.remove(path)
    print(f"Removed {path}: {server_id} was its only profile.")
    sys.exit(0)
default = profiles.get("default")
if default not in {s.get("id") for s in left}:
    default = left[0].get("id")
    print(f"{path}: the default profile is {default} now.")
tmp = path + ".tmp"
with os.fdopen(os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644), "w",
               encoding="utf-8") as f:
    json.dump(dict(profiles, default=default, servers=left), f, indent=2)
    f.write("\n")
os.chmod(tmp, os.stat(path).st_mode & 0o777)
os.replace(tmp, path)
print(f"Removed the profile {server_id} from {path}.")
EOF
fi

if server_port "$id" >/dev/null; then
	# It says what became of the server's folder: --delete-data deletes only
	# a folder it made itself.
	# shellcheck disable=SC2086 # empty or one flag
	"$HERE/server.sh" remove "$id" $delete_data
else
	echo "Removed the server $id (the server program had forgotten it already)."
fi
# Last: until then, running this again finishes the job.
rm -f "$HERE/servers/$id.env"
