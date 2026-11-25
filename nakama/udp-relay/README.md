# UDP relay

The fallback of a P2P session whose two games find no direct UDP path to each
other. Without it, such a session falls back to the relay over the Nakama
websocket, which rides TCP: one lost segment holds up everything behind it,
and the fight stutters. This program forwards the two games' datagrams over
UDP instead, so a relayed fight behaves like a direct one with a longer round
trip.

The games still try direct paths first. When a session needs a relay, each
game asks the server for a ticket (the `ikemen_relay` RPC with `udp: true`,
`nakama/modules/ikemen_relay.lua`). The module checks that the session or lobby
paired the two players, and gives each one the relay's address and a token
signed with a secret it shares with the relay. Each game sends its token to the
relay from the socket it will use; once both sides joined, the relay forwards
each side's datagrams to the other.

What it does not do:

- It never reads the payloads: the games seal them end to end, so the relay
  cannot read or change them.
- It forwards only to addresses that proved they hold a token and receive at
  that address (a join from a new address is answered with a cookie the game
  must send back), forwards what it gets byte for byte, and never answers with
  more bytes than it got: it is no amplifier, and a forged source address can
  neither take over a player's side nor point the relay's stream at a victim.
- Answers are limited per source /24 (or /64): 5 a second to datagrams without
  a valid token, 50 a second to joins with one, and 20 a second within a
  session, so junk from one place cannot starve everybody else's joins.
- An address that holds an active side of one session cannot join another
  (it is refused until that side has been silent for 15 seconds).
- Each side may send at most 500 datagrams and 256 KB a second (a fight sends
  about 125 and a few KB); more is dropped. A side silent for 60 seconds is
  forgotten. At most 4096 sessions at once.

When the server has no UDP relay configured, everything works as before: the
games use the websocket relay.

## Build

From the repository root:

```sh
go build -o ~/bin/ikemen-udp-relay ./nakama/udp-relay
```

It uses only Go's standard library, so it cross-compiles anywhere
(`GOOS=linux GOARCH=arm64 go build ...`).

## Configure

1. Make a secret (32 random characters or more; at least 16):

   ```sh
   mkdir -p ~/.config/ikemen-udp-relay
   head -c 32 /dev/urandom | base64 | tr -d '/+=' > ~/.config/ikemen-udp-relay/secret
   chmod 600 ~/.config/ikemen-udp-relay/secret
   ```

2. Tell Nakama about the relay, in the `runtime.env` of its config (or
   `--runtime.env` on its command line):

   ```yaml
   runtime:
     env:
       - "IKEMEN_UDP_RELAY_ADDR=:17360"      # ":port" = the Nakama host's address as the games know it
       - "IKEMEN_UDP_RELAY_SECRET=<the secret>"
       # - "IKEMEN_UDP_RELAY_TTL=14400"      # seconds a ticket can join (default 14400, 4 hours)
   ```

   `IKEMEN_UDP_RELAY_ADDR` can also be `relay.example.com:17360` when the relay
   runs on another host. Restart Nakama after changing it. Leaving either
   variable out (or a secret under 16 characters) turns the UDP relay off.

3. Forward the relay's UDP port (17360 by default, or whatever `-listen` says)
   on your router and open it in the firewall. Only UDP: the relay uses no TCP.

## Run as a systemd user service

`~/.config/systemd/user/ikemen-udp-relay.service`:

```ini
[Unit]
Description=IKEMEN GO UDP relay
After=network-online.target

[Service]
ExecStart=%h/bin/ikemen-udp-relay -listen :17360 -secret-file %h/.config/ikemen-udp-relay/secret
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now ikemen-udp-relay
loginctl enable-linger "$USER"   # keep it running while you are logged out
journalctl --user -u ikemen-udp-relay -f
```

## Flags

| Flag | Default | |
| --- | --- | --- |
| `-listen` | `:17360` | UDP address to listen on |
| `-secret-file` | | file with the shared secret; else `$IKEMEN_UDP_RELAY_SECRET` |
| `-idle` | `60s` | forget a side after this long without datagrams |
| `-pps` | `500` | datagrams a second each side may send |
| `-bps` | `262144` | bytes a second each side may send |
| `-max-sessions` | `4096` | sessions held at once |
| `-stats` | `5m` | log counters this often (`0`: never) |

## Protocol

Every datagram starts with a type byte. Game to relay: `J` + token [+ `|` +
cookie] (join, or join again from a new address), `D` + datagram (for the
other side), `K` + 0 (keepalive). Relay to game: `C` + cookie (32 hex digits:
join again with it, from the same address; good for 30 to 60 seconds), `A` +
state (1 waiting for the other side, 2 paired; also the answer to a
keepalive), `E` + code (1 bad token, 2 expired, 3 relay full, 4 this address
has not joined: join again, 5 this address holds another session's side),
`D` + datagram. A join from the address the side already holds needs no
cookie.

The game sends a keepalive when nothing went out or came in for 5 seconds; a
game that heard nothing from the relay for 20 seconds, or whose rejoin was
refused (its ticket expired), drops the path and asks the server for a new
ticket. The ticket's expiry is checked by the relay only, on the server's
clock; the server also sends the TTL, which the game counts from its own
clock.

A token is `v1.<session>.<side>.<expires>.<mac>`: the session is the first
32 hex digits of HMAC-SHA256(secret, `session|<match id>|<lesser user
id>|<greater user id>`), the side 0 for the lesser user id and 1 for the
other, expires a Unix time in seconds, and mac the hex HMAC-SHA256(secret, the
token up to `.<mac>`). See `relay/relay.go` (`Sign`, `SessionID`).
