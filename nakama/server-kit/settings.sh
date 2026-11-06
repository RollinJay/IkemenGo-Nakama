# Read by the kit's scripts (". settings.sh", with HERE set): the KEY=value
# lines of settings.env, without quotes, the last line for a key winning.
# Copy settings.env.example to settings.env to begin.
setting() {
	if [ -r "$HERE/settings.env" ]; then
		sed -n "s/^$1=//p" "$HERE/settings.env" | tail -n 1 | tr -d '\r'
	fi
}
SERVER_ID=$(setting SERVER_ID)
TITLE=$(setting TITLE)
GAME=$(setting GAME)
BUILD=$(setting BUILD)
REGION=$(setting REGION)
GAME_DIR=$(setting GAME_DIR)
MATCHMAKER=$(setting MATCHMAKER)
CADDY=$(setting CADDY)
[ -n "$GAME" ] || GAME=$SERVER_ID
[ -n "$REGION" ] || REGION=us-west
[ -n "$TITLE" ] || TITLE=$SERVER_ID

# Stops a script that needs the server's id while settings.env has none.
need_server_id() {
	if ! printf '%s' "$SERVER_ID" | grep -Eq '^[a-z0-9][a-z0-9-]*$'; then
		echo "settings.env: set SERVER_ID to the server's id (lowercase letters, digits and dashes); see README.md" >&2
		exit 2
	fi
}
