#!/bin/sh
# Installs this folder's services for systemd, run as you (not root), and the
# window in the app menu:
#   <id>-server               one per server (servers/<id>.env): its Nakama
#                             and database (server.sh run <id>)
#   <KIT_ID>-caddy            HTTPS in front of all of them (run-caddy.sh);
#                             it waits until set-public-host.sh has written a
#                             site (caddy/sites/<id>.caddy)
#   <KIT_ID>-duckdns.timer    ddns/duckdns-update.sh every 5 minutes; it skips
#                             its runs until ddns/duckdns.env exists
# KIT_ID is settings.env's (the first server's id until set).
#
#   ./install-services.sh                install them all, and start them
#   ./install-services.sh --no-start     install and enable them, start none
#   ./install-services.sh --server <id>  one server's service only (add-server.sh)
#   ./install-services.sh --remove       stop them all and remove them
# It refuses to replace a service of the same name that another folder
# installed.
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"

remove= start=1 only=
while [ $# -gt 0 ]; do
	case $1 in
	--remove) remove=1 ;;
	--no-start) start= ;;
	--server)
		[ $# -ge 2 ] || { echo "--server needs the server's id" >&2; exit 2; }
		only=$2
		shift
		;;
	*)
		echo "usage: $0 [--remove] [--no-start] [--server <id>]" >&2
		exit 2
		;;
	esac
	shift
done

# Units and launchers would need quoting rules of their own for anything else.
case $HERE in
*[!A-Za-z0-9._/-]*)
	echo "Move this folder to a path of letters, digits, dots, dashes and slashes only: '$HERE'" >&2
	exit 2
	;;
esac
kit=$KIT_ID
# The window's application id, as server-control.py makes it.
part=$(printf '%s' "$kit" | tr -c 'A-Za-z0-9' '_')
case $part in [0-9]*) part=s_$part ;; esac
app_id=local.IkemenServer.$part

if [ -n "$remove" ]; then
	# Every server service of this folder, also of servers removed since.
	units=
	for file in "$UNITS"/*-server.service; do
		[ -f "$file" ] || continue
		if grep -q "^ExecStart=$HERE/server.sh run " "$file"; then
			units="$units ${file##*/}"
		fi
	done
	if [ -n "$kit" ]; then
		for unit in "$kit-caddy.service" "$kit-duckdns.service" "$kit-duckdns.timer"; do
			if [ -f "$UNITS/$unit" ] && ! unit_elsewhere "$unit"; then
				units="$units $unit"
			fi
		done
	fi
	if [ -n "$units" ]; then
		# shellcheck disable=SC2086 # unit names have no spaces
		systemctl --user disable --now $units 2>/dev/null || true
		for unit in $units; do
			rm -f "$UNITS/$unit"
		done
	fi
	rm -f "$APPS/$app_id.desktop"
	systemctl --user daemon-reload
	echo "Removed the services and the launcher:${units:- none were installed}. The servers themselves (config/) are still here."
	exit 0
fi

python=$(command -v python3) || python=/usr/bin/python3
# systemd reads % as the start of a specifier.
quote_title() {
	printf '%s' "$1" | sed 's/%/%%/g'
}

# Stops before writing anything when a server is not ready, or a unit of the
# same name is another folder's.
check_server() {
	need_server_id "$1"
	if ! server_port "$1" >/dev/null; then
		echo "The server program has no server '$1' (./server.sh list): ./add-server.sh $1 makes it" >&2
		exit 2
	fi
	check_unit "$1-server.service"
}
check_unit() {
	if unit_elsewhere "$1"; then
		echo "$UNITS/$1 is another folder's ($(sed -n 's/^ExecStart=//p' "$UNITS/$1" | head -n 1)): not replacing it" >&2
		exit 2
	fi
}

write_server_unit() {
	load_server "$1"
	cat > "$UNITS/$1-server.service" <<EOF
[Unit]
Description=$(quote_title "$S_TITLE"): Nakama server and its database (nakama-matchmaker-server)
Documentation=file://$HERE/README.md

[Service]
ExecStart=$HERE/server.sh run $1
Restart=on-failure
RestartSec=10
# The program stops Nakama, then the database: let it, and give it the time.
KillMode=mixed
TimeoutStopSec=60

[Install]
WantedBy=default.target
EOF
}

mkdir -p "$UNITS" "$APPS"

if [ -n "$only" ]; then
	check_server "$only"
	write_server_unit "$only"
	systemctl --user daemon-reload
	systemctl --user enable "$only-server.service"
	if [ -n "$start" ]; then
		systemctl --user start "$only-server.service"
		echo "Installed and started $only-server."
	else
		echo "Installed $only-server; it starts with this PC."
	fi
	exit 0
fi

ids=$(server_ids)
if [ -z "$ids" ]; then
	echo "There is no server yet: ./add-server.sh <id> creates one (README.md)" >&2
	exit 2
fi
for id in $ids; do
	check_server "$id"
done
for unit in "$kit-caddy.service" "$kit-duckdns.service" "$kit-duckdns.timer"; do
	check_unit "$unit"
done
# The shared services' name must not change when servers come and go.
[ -n "$(setting KIT_ID)" ] || set_env_value "$HERE/settings.env" KIT_ID "$kit"
title=$(quote_title "$TITLE")

server_units=
for id in $ids; do
	write_server_unit "$id"
	server_units="$server_units $id-server.service"
done

cat > "$UNITS/$kit-caddy.service" <<EOF
[Unit]
Description=$title: HTTPS (Caddy) in front of the servers
Documentation=file://$HERE/README.md
After=$server_units
# Caddy needs a public name; set-public-host.sh writes each server's site.
ConditionPathExistsGlob=$HERE/caddy/sites/*.caddy

[Service]
ExecStart=$HERE/run-caddy.sh
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF

cat > "$UNITS/$kit-duckdns.service" <<EOF
[Unit]
Description=$title: point the DuckDNS names at this network's address
Documentation=file://$HERE/README.md
ConditionPathExists=$HERE/ddns/duckdns.env

[Service]
Type=oneshot
ExecStart=$HERE/ddns/duckdns-update.sh
EOF

cat > "$UNITS/$kit-duckdns.timer" <<EOF
[Unit]
Description=$title: update the DuckDNS names every 5 minutes

[Timer]
OnBootSec=1min
OnUnitActiveSec=5min

[Install]
WantedBy=timers.target
EOF

cat > "$APPS/$app_id.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=$TITLE
Comment=Start, stop and set up the game's online servers
Exec=$python -I $HERE/server-control.py
Icon=network-server
Terminal=false
Categories=Network;
StartupNotify=true
StartupWMClass=$app_id
EOF

systemctl --user daemon-reload
# shellcheck disable=SC2086 # unit names have no spaces
systemctl --user enable $server_units "$kit-caddy.service" "$kit-duckdns.timer"
if [ -z "$start" ]; then
	echo "Installed and enabled$server_units, $kit-caddy and $kit-duckdns.timer (none started),"
	echo "and \"$TITLE\" in the app menu."
	exit 0
fi
# shellcheck disable=SC2086
systemctl --user start $server_units "$kit-duckdns.timer"
for site in "$HERE"/caddy/sites/*.caddy; do
	if [ -f "$site" ]; then
		systemctl --user start "$kit-caddy.service"
		break
	fi
done
echo "Installed and started$server_units, $kit-caddy (once there is a public name) and $kit-duckdns.timer,"
echo "and \"$TITLE\" in the app menu."
echo "To keep them running while you are logged out: loginctl enable-linger $USER"
