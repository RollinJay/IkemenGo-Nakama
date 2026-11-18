# Read by the kit's scripts (". settings.sh", with HERE set).
#
# settings.env holds what the whole folder shares (KIT_ID, TITLE, MATCHMAKER,
# CADDY), and servers/<id>.env one server each (TITLE, GAME_DIR, GAME, BUILD,
# REGION, PUBLIC_HOST): KEY=value lines, without quotes, the last line for a
# key winning. Copy settings.env.example to settings.env to begin, and add
# servers with add-server.sh. (The functions' own variables start with _:
# sh has no local ones.)

# A KEY=value file's value for a key.
env_value() {
	if [ -r "$1" ]; then
		sed -n "s/^$2=//p" "$1" | tail -n 1 | tr -d '\r'
	fi
}
setting() {
	env_value "$HERE/settings.env" "$1"
}
server_setting() {
	env_value "$HERE/servers/$1.env" "$2"
}

# Sets KEY=value in a KEY=value file: one line for the key, where its first
# line was, or at the end.
set_env_value() {
	mkdir -p "$(dirname -- "$1")"
	touch "$1"
	ENV_KEY=$2 ENV_VALUE=$3 awk '
		BEGIN { key = ENVIRON["ENV_KEY"]; line = key "=" ENVIRON["ENV_VALUE"] }
		index($0, key "=") == 1 { if (!done) print line; done = 1; next }
		{ print }
		END { if (!done) print line }
	' "$1" > "$1.tmp.$$"
	# Over the file, which keeps its permissions.
	cat "$1.tmp.$$" > "$1"
	rm -f "$1.tmp.$$"
}
set_server_setting() {
	set_env_value "$HERE/servers/$1.env" "$2" "$3"
}

valid_id() {
	printf '%s' "$1" | grep -Eq '^[a-z0-9][a-z0-9-]*$'
}

# The servers, one id per line: the names of servers/*.env.
server_ids() {
	for _file in "$HERE"/servers/*.env; do
		[ -f "$_file" ] || continue
		_file=${_file##*/}
		valid_id "${_file%.env}" && printf '%s\n' "${_file%.env}"
	done | LC_ALL=C sort
}

# The folder's id: it names the shared services (<KIT_ID>-caddy,
# <KIT_ID>-duckdns) and the window. Until settings.env has one, the first
# server's id.
kit_id() {
	_kit=$(setting KIT_ID)
	[ -n "$_kit" ] || _kit=$(server_ids | head -n 1)
	printf '%s' "$_kit"
}

# Stops a script given an id that names no server of this folder.
need_server_id() {
	if ! valid_id "$1"; then
		echo "'$1' is not a server id (lowercase letters, digits and dashes)" >&2
		exit 2
	fi
	if [ ! -f "$HERE/servers/$1.env" ]; then
		echo "There is no server '$1' here (servers/$1.env). The servers: $(server_ids | tr '\n' ' ')" >&2
		exit 2
	fi
}

# The server a script works on: the id given, or else the only server there
# is. Use it as id=$(pick_server "$1") || exit.
pick_server() {
	if [ -n "$1" ]; then
		need_server_id "$1"
		printf '%s' "$1"
		return
	fi
	_ids=$(server_ids)
	if [ -z "$_ids" ]; then
		echo "There is no server yet: ./add-server.sh <id> creates one (README.md)" >&2
		exit 2
	fi
	if [ "$(printf '%s\n' "$_ids" | wc -l)" -ne 1 ]; then
		echo "Name the server, one of: $(printf '%s' "$_ids" | tr '\n' ' ')" >&2
		exit 2
	fi
	printf '%s' "$_ids"
}

# A server's settings, with their defaults: S_TITLE, S_GAME_DIR, S_GAME,
# S_BUILD, S_REGION and S_PUBLIC_HOST.
load_server() {
	S_TITLE=$(server_setting "$1" TITLE)
	S_GAME_DIR=$(server_setting "$1" GAME_DIR)
	S_GAME=$(server_setting "$1" GAME)
	S_BUILD=$(server_setting "$1" BUILD)
	S_REGION=$(server_setting "$1" REGION)
	S_PUBLIC_HOST=$(server_setting "$1" PUBLIC_HOST)
	[ -n "$S_TITLE" ] || S_TITLE=$1
	[ -n "$S_GAME" ] || S_GAME=$1
	[ -n "$S_REGION" ] || S_REGION=us-west
}

# A server's API port, or with "console_port" its console's, as
# nakama-matchmaker-server keeps them in config/servers.json.
server_port() {
	python3 -I - "$HERE/config/servers.json" "$1" "${2:-port}" <<'EOF'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as f:
        servers = json.load(f).get("servers", [])
except (OSError, ValueError):
    servers = []
for s in servers:
    if s.get("id") == sys.argv[2] and s.get(sys.argv[3]):
        print(s[sys.argv[3]])
        sys.exit(0)
sys.exit(1)
EOF
}

UNITS=${SYSTEMD_USER_DIR:-$HOME/.config/systemd/user}
APPS=${APPLICATIONS_DIR:-$HOME/.local/share/applications}

# Whether a unit file of that name belongs to another folder: it runs a
# program outside this one (a timer: its service does). Every folder of the
# same user shares unit names.
unit_elsewhere() {
	_unit=$1
	case $_unit in *.timer) _unit=${_unit%.timer}.service ;; esac
	[ -f "$UNITS/$_unit" ] || return 1
	_start=$(sed -n 's/^ExecStart=//p' "$UNITS/$_unit" | head -n 1)
	case $_start in
	"$HERE"/*) return 1 ;;
	esac
	return 0
}

# Whether this folder's shared Caddy service is installed and starts with
# the PC: a <KIT_ID>-caddy of another folder is not this one's to restart.
caddy_enabled() {
	[ -n "$KIT_ID" ] && ! unit_elsewhere "$KIT_ID-caddy.service" &&
		systemctl --user is-enabled --quiet "$KIT_ID-caddy" 2>/dev/null
}

# Whether install-services.sh has installed this folder's services.
services_installed() {
	_kit=$(kit_id)
	[ -n "$_kit" ] && [ -f "$UNITS/$_kit-caddy.service" ] && ! unit_elsewhere "$_kit-caddy.service"
}

# A folder set up for a single server (SERVER_ID in settings.env, and no
# servers/ yet) becomes this layout first. The upgrade turns that line into a
# comment last, so one that stopped halfway goes on the next time.
if [ -z "$SERVER_KIT_UPGRADING" ] && [ -n "$(setting SERVER_ID)" ]; then
	"$HERE/upgrade-kit.sh" >&2 || exit 1
fi

MATCHMAKER=$(setting MATCHMAKER)
CADDY=$(setting CADDY)
KIT_ID=$(kit_id)
# It names unit files: a stray character would put them elsewhere.
if [ -n "$KIT_ID" ] && ! valid_id "$KIT_ID"; then
	echo "settings.env: KIT_ID '$KIT_ID' is not an id (lowercase letters, digits and dashes)" >&2
	exit 2
fi
TITLE=$(setting TITLE)
[ -n "$TITLE" ] || TITLE=${KIT_ID:-Game server}
