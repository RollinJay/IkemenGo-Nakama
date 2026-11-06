#!/bin/sh
# Keeps a DuckDNS name pointing at this network's public IP address, so the
# community's games still find the server after your ISP changes the address.
#
# Put the name (without .duckdns.org) and your token from https://www.duckdns.org
# in ddns/duckdns.env next to this script, and make it readable only by you
# (chmod 600 ddns/duckdns.env):
#   DUCKDNS_DOMAIN=mygame
#   DUCKDNS_TOKEN=<your token>
# Then run this script once; the <id>-duckdns timer (install-services.sh) runs it every 5 minutes.
set -e
HERE=$(CDPATH= cd -- "$(dirname -- "$(readlink -f -- "$0")")" && pwd)
ENV_FILE="$HERE/duckdns.env"
if [ ! -r "$ENV_FILE" ]; then
	echo "missing $ENV_FILE (see the top of this script)" >&2
	exit 2
fi
# The last line for each name wins; surrounding quotes and spaces are dropped.
value() {
	sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*//p" "$ENV_FILE" | tail -n 1 |
		tr -d '\r' | sed -e 's/[[:space:]]*$//' -e "s/^['\"]//" -e "s/['\"]\$//"
}
DUCKDNS_DOMAIN=$(value DUCKDNS_DOMAIN)
DUCKDNS_TOKEN=$(value DUCKDNS_TOKEN)
DUCKDNS_DOMAIN=${DUCKDNS_DOMAIN%.duckdns.org}
if [ -z "$DUCKDNS_DOMAIN" ] || [ -z "$DUCKDNS_TOKEN" ]; then
	echo "$ENV_FILE needs DUCKDNS_DOMAIN and DUCKDNS_TOKEN" >&2
	exit 2
fi
case $DUCKDNS_DOMAIN$DUCKDNS_TOKEN in
	*[!A-Za-z0-9.-]*)
		echo "$ENV_FILE: the name and token are letters, digits, dots and dashes only" >&2
		exit 2
		;;
esac

# DuckDNS takes the token in the URL. curl reads the URL from its standard
# input, so the token does not show in the process list. An empty ip= lets
# DuckDNS use the address the request comes from.
answer=$(printf 'url = "https://www.duckdns.org/update?domains=%s&token=%s&ip="\n' \
	"$DUCKDNS_DOMAIN" "$DUCKDNS_TOKEN" | curl -fsS --max-time 30 --config -)
if [ "$answer" != OK ]; then
	echo "DuckDNS did not update $DUCKDNS_DOMAIN.duckdns.org (answer: $answer)" >&2
	exit 1
fi
echo "$DUCKDNS_DOMAIN.duckdns.org points at this network's address"
