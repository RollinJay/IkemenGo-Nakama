#!/bin/sh
# Installs this folder's services for systemd, run as you (not root) and named
# after SERVER_ID in settings.env, and the window in the app menu:
#   <id>-server          the server: Nakama and its database (server.sh run <id>)
#   <id>-caddy           HTTPS in front of it (run-caddy.sh); it waits until
#                        set-public-host.sh has written caddy/public-host.env
#   <id>-duckdns.timer   ddns/duckdns-update.sh every 5 minutes; it skips its
#                        runs until ddns/duckdns.env exists
#
#   ./install-services.sh            install them, and start the server
#   ./install-services.sh --remove   stop them and remove them
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
. "$HERE/settings.sh"
need_server_id
id=$SERVER_ID
# Units and launchers would need quoting rules of their own for anything else.
case $HERE in
*[!A-Za-z0-9._/-]*)
	echo "Move this folder to a path of letters, digits, dots, dashes and slashes only: '$HERE'" >&2
	exit 2
	;;
esac
UNITS=${SYSTEMD_USER_DIR:-$HOME/.config/systemd/user}
APPS=${APPLICATIONS_DIR:-$HOME/.local/share/applications}
# The window's application id, as server-control.py makes it.
part=$(printf '%s' "$id" | tr -c 'A-Za-z0-9' '_')
case $part in [0-9]*) part=s_$part ;; esac
app_id=local.IkemenServer.$part

if [ "$1" = --remove ]; then
	systemctl --user disable --now "$id-caddy.service" "$id-duckdns.timer" "$id-server.service" 2>/dev/null || true
	rm -f "$UNITS/$id-server.service" "$UNITS/$id-caddy.service" "$UNITS/$id-duckdns.service" \
		"$UNITS/$id-duckdns.timer" "$APPS/$app_id.desktop"
	systemctl --user daemon-reload
	echo "Removed the services and the launcher. The server itself (config/) is still here."
	exit 0
fi

if ! "$HERE/server.sh" show "$id" >/dev/null 2>&1; then
	echo "There is no server $id yet: create it first (./server.sh add $id ..., README.md)" >&2
	exit 2
fi
python=$(command -v python3) || python=/usr/bin/python3
# systemd reads % as the start of a specifier.
title=$(printf '%s' "$TITLE" | sed 's/%/%%/g')
mkdir -p "$UNITS" "$APPS"

cat > "$UNITS/$id-server.service" <<EOF
[Unit]
Description=$title: Nakama server and its database (nakama-matchmaker-server)
Documentation=file://$HERE/README.md

[Service]
ExecStart=$HERE/server.sh run $id
Restart=on-failure
RestartSec=10
# The program stops Nakama, then the database: let it, and give it the time.
KillMode=mixed
TimeoutStopSec=60

[Install]
WantedBy=default.target
EOF

cat > "$UNITS/$id-caddy.service" <<EOF
[Unit]
Description=$title: HTTPS (Caddy) in front of the server
Documentation=file://$HERE/README.md
After=$id-server.service
# Caddy needs the public name; set-public-host.sh writes it.
ConditionPathExists=$HERE/caddy/public-host.env

[Service]
ExecStart=$HERE/run-caddy.sh
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF

cat > "$UNITS/$id-duckdns.service" <<EOF
[Unit]
Description=$title: point the DuckDNS name at this network's address
Documentation=file://$HERE/README.md
ConditionPathExists=$HERE/ddns/duckdns.env

[Service]
Type=oneshot
ExecStart=$HERE/ddns/duckdns-update.sh
EOF

cat > "$UNITS/$id-duckdns.timer" <<EOF
[Unit]
Description=$title: update the DuckDNS name every 5 minutes

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
Comment=Start, stop and set up the game's online server
Exec=$python -I $HERE/server-control.py
Icon=network-server
Terminal=false
Categories=Network;
StartupNotify=true
StartupWMClass=$app_id
EOF

systemctl --user daemon-reload
systemctl --user enable "$id-server.service" "$id-caddy.service" "$id-duckdns.timer"
systemctl --user start "$id-server.service" "$id-duckdns.timer"
if [ -r "$HERE/caddy/public-host.env" ]; then
	systemctl --user start "$id-caddy.service"
fi
echo "Installed and started $id-server, $id-caddy (once there is a public name) and $id-duckdns.timer,"
echo "and \"$TITLE\" in the app menu."
echo "To keep them running while you are logged out: loginctl enable-linger $USER"
