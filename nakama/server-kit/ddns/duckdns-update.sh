#!/bin/sh
# Keeps DuckDNS names pointing at this network's public IP address, so the
# community's games still find the servers after your ISP changes the address.
#
# The names are every server's public name that ends in .duckdns.org
# (PUBLIC_HOST in servers/<id>.env, which set-public-host.sh saves), and any
# more in DUCKDNS_DOMAIN. Put your token from https://www.duckdns.org in
# ddns/duckdns.env next to this script, and make it readable only by you
# (chmod 600 ddns/duckdns.env):
#   DUCKDNS_TOKEN=<your token>
#   DUCKDNS_DOMAIN=othername,another     (optional, without .duckdns.org)
# Then run this script once; the <KIT_ID>-duckdns timer (install-services.sh)
# runs it every 5 minutes. All the names go in one request.
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
KIT=$(dirname -- "$HERE")
ENV_FILE="$HERE/duckdns.env"
if [ ! -r "$ENV_FILE" ]; then
	echo "missing $ENV_FILE (see the top of this script)" >&2
	exit 2
fi
# The last line for each name wins; surrounding quotes and spaces are dropped.
value() {
	sed -n "s/^[[:space:]]*$2[[:space:]]*=[[:space:]]*//p" "$1" | tail -n 1 |
		tr -d '\r' | sed -e 's/[[:space:]]*$//' -e "s/^['\"]//" -e "s/['\"]\$//"
}
DUCKDNS_TOKEN=$(value "$ENV_FILE" DUCKDNS_TOKEN)

# DuckDNS updates its own names: the label just before .duckdns.org
# (play.mygame.duckdns.org is mygame's).
names=$(
	{
		value "$ENV_FILE" DUCKDNS_DOMAIN | tr ', ' '\n\n' | sed -e '/^$/d' -e '/\.duckdns\.org$/!s/$/.duckdns.org/'
		for file in "$KIT"/servers/*.env; do
			if [ -f "$file" ]; then
				value "$file" PUBLIC_HOST
				echo
			fi
		done
	} | tr 'A-Z' 'a-z' | sed -n 's/^\(.*\.\)\{0,1\}\([^.][^.]*\)\.duckdns\.org$/\2/p' | LC_ALL=C sort -u
)
domains=$(printf '%s' "$names" | tr '\n' ',')
if [ -z "$domains" ] || [ -z "$DUCKDNS_TOKEN" ]; then
	echo "$ENV_FILE needs DUCKDNS_TOKEN, and a name: a server's public name ending in .duckdns.org, or DUCKDNS_DOMAIN" >&2
	exit 2
fi
case $domains$DUCKDNS_TOKEN in
	*[!A-Za-z0-9,-]*)
		echo "$ENV_FILE: the names and token are letters, digits and dashes only (names: $domains)" >&2
		exit 2
		;;
esac

# DuckDNS takes the token in the URL. curl reads the URL from its standard
# input, so the token does not show in the process list. An empty ip= lets
# DuckDNS use the address the request comes from.
answer=$(printf 'url = "https://www.duckdns.org/update?domains=%s&token=%s&ip="\n' \
	"$domains" "$DUCKDNS_TOKEN" | curl -fsS --max-time 30 --config -)
if [ "$answer" != OK ]; then
	echo "DuckDNS did not update $domains (answer: $answer)" >&2
	exit 1
fi
echo "$(printf '%s\n' "$names" | sed 's/$/.duckdns.org/' | tr '\n' ' ')point at this network's address"
