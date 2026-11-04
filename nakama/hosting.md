# Running a server for your game, step by step

This guide is for a developer who ships a game built on this fork and wants its players to play online. By the end, the game's players sign in to your server at a name such as `mygame.example.org` over HTTPS, find each other in Quick Match and lobbies, and play; your game ships a server profile, `data/online/servers.json`, that sends every copy of the game to that server with no setting to enter.

The server keeps the accounts, online names, matchmaking, lobbies, chat, ratings and the ranked sets' records. It does not run the fights: the two players' games connect to each other and play over that connection (see [The connection ladder](README.md#the-connection-ladder)). When no direct connection works, the fight runs through the server instead, over the same connection the game already has to it; that needs no setting either.

The steps follow a setup that has run a real community (Hyper DBZ) on a home Linux PC: [Nakama](https://heroiclabs.com/nakama/) and its database, run by [Nakama Matchmaker](https://github.com/RollinJay/Nakama-Matchmaker)'s `nakama-matchmaker-server`, behind [Caddy](https://caddyserver.com/) for HTTPS. Nakama Matchmaker's [hosting guide](https://github.com/RollinJay/Nakama-Matchmaker/blob/main/docs/hosting.md) describes its every option; this guide is the path from nothing to players online.

| Piece | What it does |
|---|---|
| Nakama 3 (tested with 3.41.0) | the game server the fork's online play talks to |
| PostgreSQL | Nakama's database |
| `nakama-matchmaker-server` | sets up and runs Nakama and a PostgreSQL of its own: random keys, this fork's server modules, the settings they need, the console and the database on this computer only |
| Caddy | HTTPS in front of Nakama, with a free Let's Encrypt certificate |
| a name | a domain of yours, or a free dynamic DNS name such as DuckDNS |
| a router or a firewall | one TCP port, 443, open to the internet |

**Why not `Ikemen_GO -server`?** The fork's own launcher ([Optional dedicated-server launcher](README.md#optional-dedicated-server-launcher)) starts Nakama with Nakama's defaults: the public HTTP key `defaulthttpkey`, which lets anyone call the moderators' RPCs, the web console open on every address with the user `admin` and the password `password`, no path to this fork's modules, and a server profile without STUN servers. It is fine for trying things on your own network; do not open it to the internet.

## 1. Choose where the server runs

- **A Linux PC at home.** It costs nothing more, but: the PC must be on whenever people play; your router must let you forward a port; your connection needs a public IPv4 address (if the address your router shows for its internet side differs from the one a "what is my IP" site shows, or starts with 100.64 to 100.127, your provider shares one address among many homes and you cannot host at home); and your server's name points at your home connection, so everyone can see your home's IP address, and an attack on the server is an attack on your home internet.
- **A rented Linux server (VPS).** Always on, with a fixed address that is not your home's, and no router to set up; a small one is enough to start. Watch its memory and disk as the community grows: lobbies being watched keep their matches' streams (up to 8 MB each), and the ranked sets' replays are kept for 30 days by default.

Linux is what has been tested (Debian and Ubuntu). Nakama Matchmaker's Windows and macOS code exists but has not run.

## 2. Get the programs

Run everything as an ordinary user, not root (PostgreSQL refuses to run as root).

1. **Nakama**: download `nakama-3.41.0-linux-amd64.tar.gz` from [Nakama's releases](https://github.com/heroiclabs/nakama/releases) and unpack the `nakama` program, for example to `~/server/nakama/nakama`.
2. **PostgreSQL**: install your distribution's package (`sudo apt install postgresql` on Debian and Ubuntu). Only its programs are used (`initdb`, `postgres`, `pg_ctl`, `pg_isready`, in `/usr/lib/postgresql/<version>/bin`); the server runs a database of its own, so the system's PostgreSQL service can stay off.
3. **Nakama Matchmaker's server program**: download `nakama-matchmaker-server-linux-x86_64.tar.gz` from [Nakama Matchmaker's releases](https://github.com/RollinJay/Nakama-Matchmaker/releases) and unpack it, for example to `~/server/matchmaker`. The program is `bin/nakama-matchmaker-server`; it brings this fork's server modules with it.
4. **Caddy**: download `caddy_<version>_linux_amd64.tar.gz` from [Caddy's releases](https://github.com/caddyserver/caddy/releases) and unpack the `caddy` program, for example to `~/server/caddy/caddy`.

Tell the server program where Nakama is (it finds PostgreSQL's programs where the package installs them; `--postgres <folder>` names them otherwise):

```bash
~/server/matchmaker/bin/nakama-matchmaker-server programs --nakama ~/server/nakama/nakama
```

The commands below shorten the program's path to `nakama-matchmaker-server`. It keeps its settings in `~/.config/nakama-matchmaker` (`--config <folder>`, given before the command, chooses another folder).

## 3. Create the server

Pick an id for the server (lowercase letters, digits and dashes) and the name players will reach it by:

```bash
nakama-matchmaker-server add mygame --kinds ikemen --title "My Game Online" --bind 127.0.0.1 --public-host mygame.example.org --public-port 443 --client-tls yes
```

- `--kinds ikemen` installs this fork's server modules and Nakama Matchmaker's community module, which stops players from writing the records the modules keep (such as their own rating).
- `--bind 127.0.0.1` makes Nakama listen on this computer only: players reach it through Caddy, never directly.
- `--public-host`, `--public-port 443` and `--client-tls yes` are what players are told: your name, Caddy's port, HTTPS.
- Every server gets its own random keys and passwords, kept in files only you can read (mode 600).

Optional settings, at creation or later with `set mygame ...` (a running server takes them at its next start):

- **Levels** players see for their ratings, such as "Power level 9,001" for a new player: `--level-start 9001 --level-top 1e9 --level-label "Power level"` ([Levels](README.md#levels)).
- **Ranked settings**, as Nakama runtime values ([Ranked settings](README.md#ranked-settings)), for example `--nakama-arg=--runtime.env --nakama-arg=IKEMEN_RANKED_REPLAY_DAYS=7`.

Start it once in a terminal:

```bash
NAKAMA_TELEMETRY=0 nakama-matchmaker-server run mygame
```

The first start creates the database, which takes a few seconds; the log then says that the server is running. `NAKAMA_TELEMETRY=0` stops Nakama from sending anonymous usage statistics to its makers. In another terminal, `curl http://127.0.0.1:7350/healthcheck` answers `{}`. `nakama-matchmaker-server show mygame` prints what players get; `show mygame --secrets` adds the console's password and the other keys, which are yours alone. Stop the server with Ctrl+C.

## 4. Give the server a name

- **With a domain of your own**, add an `A` record for the name (`mygame.example.org`) pointing at your public IPv4 address: your VPS's, or your home connection's.
- **At home, without a domain**, a free dynamic DNS name such as `mygame.duckdns.org` follows your home address when your provider changes it. [DuckDNS](https://www.duckdns.org/install.jsp) shows how to keep it updated (a script run every five minutes).

## 5. Open one port

Players need **TCP port 443** and nothing else: Nakama's port, its console and the database stay on the server. The fights need no port on the server at all: they go between the players' games (UDP), or through the server's port 443 when nothing else works.

- **At home**: give the PC a fixed address on your network (a DHCP reservation in the router), then forward the router's public **TCP 443** to that PC's port **8443**, where Caddy listens below (an ordinary user cannot listen on 443). Forward nothing else.
- **On a VPS**: allow incoming TCP 443 in its firewall, and nothing else that players need. Caddy may listen on 443 there itself (drop the `https_port` line below, and let your distribution's Caddy service run it).

## 6. Put HTTPS in front with Caddy

Save this as `~/server/Caddyfile`, with your name in place of `mygame.example.org`:

```caddyfile
{
	# At home: the router forwards public 443 to this port.
	https_port 8443
	http_port 8080
	admin off
	storage file_system {$HOME}/server/caddy-data
	# The game uses HTTPS only, and Let's Encrypt checks the name through
	# port 443 (TLS-ALPN), so port 80 is not needed.
	auto_https disable_redirects
	cert_issuer acme {
		disable_http_challenge
	}
	# An RSA key: its certificate chains to the root every player's system
	# trusts, older ones included.
	key_type rsa2048
	servers {
		# No HTTP/3, which would need UDP on the same port.
		protocols h1 h2
	}
}

mygame.example.org {
	# Nakama's HTTP key lets a caller act as the server (the moderators'
	# RPCs); only programs on this computer use it, so refuse it from the
	# internet, in the query string and in Basic auth on RPC calls.
	@http_key expression `{query.http_key} != ""`
	respond @http_key 403
	@rpc_basic {
		path /v2/rpc*
		header_regexp Authorization (?i)^\s*basic\s
	}
	respond @rpc_basic 403

	reverse_proxy 127.0.0.1:7350
}
```

With the server running, run Caddy in another terminal:

```bash
~/server/caddy/caddy run --config ~/server/Caddyfile --adapter caddyfile
```

On its first start Caddy gets the certificate, which needs port 443 forwarded already. Then, from a device on another network (a phone on mobile data), open `https://mygame.example.org/healthcheck`: it shows `{}`.

## 7. Keep it running

On Linux, systemd user services start the server and Caddy at boot and restart them when they fail. Save these two files in `~/.config/systemd/user/`, with your paths:

`mygame-server.service`:

```ini
[Unit]
Description=My Game Online: Nakama server and its database

[Service]
Environment=NAKAMA_TELEMETRY=0
ExecStart=%h/server/matchmaker/bin/nakama-matchmaker-server run mygame
Restart=on-failure
RestartSec=10
# The program stops Nakama, then the database: let it, and give it the time.
KillMode=mixed
TimeoutStopSec=60

[Install]
WantedBy=default.target
```

`mygame-caddy.service`:

```ini
[Unit]
Description=My Game Online: HTTPS in front of the server
After=mygame-server.service

[Service]
ExecStart=%h/server/caddy/caddy run --config %h/server/Caddyfile --adapter caddyfile
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

Then enable them, and let them run while you are not logged in (some systems ask for an administrator's password for the second command):

```bash
systemctl --user enable --now mygame-server mygame-caddy
```

```bash
loginctl enable-linger $USER
```

## 8. Point your game at the server

Write the server profile your game ships:

```bash
nakama-matchmaker-server ikemen-profile mygame data/online/servers.json --game mygame --build mygame-1.0 --region us-west
```

It holds the server's public name, port 443, HTTPS, the server key and two public STUN servers (`--stun` names others). The server key is not a password: every copy of the game carries it. The fields that decide who plays whom ([Shipped client server profiles](README.md#shipped-client-server-profiles)):

- **`game`** names your game's pools, ratings and leaderboard. Keep it the same for the game's whole life, or the players' ratings start over.
- **`build`** keeps different versions of your game apart: only copies with the same build are matched or join the same lobby. Change it whenever two versions could not play each other: new characters, stages or screenpack, `save/config.ini`'s `[Common]` section, the scripts, or the engine (an engine whose connection protocol changed, such as this fork's version 2 handshake, cannot connect to an older one at all).
- **`region`** splits the pools too: players of different regions are never matched. Use one region until your community is large.

Your game also needs a screenpack with the online menu items (QUICK MATCH (ONLINE), ONLINE LOBBIES and, if you want ranked play, RANKED MATCH (ONLINE)); see [Screenpack integration](README.md#screenpack-integration).

## 9. Test before you share it

1. Give a copy of the game to someone on another network, or put a second PC on a phone's hotspot.
2. Both sign in and choose online names.
3. Quick Match: both get matched, play a fight, and see the same result.
4. Lobbies: one creates a lobby, the other joins it by its room ID, and they play.
5. The game's log names the way each fight connected: `Nakama P2P: connected to the other player over direct ...` (or `ipv6`, or `relay`).

## 10. Share the game

Ship the game folder with your `data/online/servers.json`, and without:

- `save/nakama_device_id`: the sign-in of your own copy. Shipped, every player would sign in as your account.
- the server's configuration folder (`~/.config/nakama-matchmaker`), its keys, the Caddyfile's data, and any `save/server.json` or `save/server-client.json`;
- your own `save/` state: logs, replays, `ranked_pending.json`, `stats.json`, the CBR AI's learned data.

## 11. Day to day

- **Logs**: `~/.config/nakama-matchmaker/servers/mygame/server.log`, and `journalctl --user -u mygame-server`.
- **Moderation**: Nakama's web console looks up and bans accounts and deletes messages. It listens on the server only; reach it through an SSH tunnel (`ssh -N -L 7351:127.0.0.1:7351 you@server`, then `http://127.0.0.1:7351`, with the user and password `show mygame --secrets` prints). The ranked sets: `nakama-matchmaker-server ranked mygame`, `ranked-replay` and `ranked-settle` ([IKEMEN GO's ranked sets](https://github.com/RollinJay/Nakama-Matchmaker/blob/main/docs/hosting.md#ikemen-gos-ranked-sets)).
- **Backups**: stop the server and copy its folder (`~/.config/nakama-matchmaker`, which holds the database and the keys); keep a copy on another computer, and try restoring one now and then.
- **Updates**: keep the system, Nakama, PostgreSQL and Caddy up to date. A new release of Nakama Matchmaker brings this fork's server modules as they are then: update the server before you ship a game build that needs newer modules.
- **Moving the server**: the players' profiles name the server by its name, not its address, so move the name's `A` record (or DuckDNS) with it, and the copies already shared keep working.

## 12. Security checklist

For you, as the server's host:

- Only TCP 443 is open to the internet. Nakama listens on 127.0.0.1; its console and the database listen on this computer only.
- The configuration folder holds the keys: keep it private (the program writes it readable by you alone), out of the game you ship and out of any repository.
- The server key is public by design. The HTTP key is not: it allows the moderators' RPCs, and Caddy refuses it from the internet.
- Anyone can create accounts, as many as they like, and pick their names; there is no limit on how fast. Watch the console, and ban whoever abuses it.
- The server sees what players do on it: accounts, names, chat, matchmaking, ranked claims and the ranked sets' replays. It does not see the fights: they are sealed between the two players' games, and when the server relays a fight it passes on what it cannot read (roughly a few tens of kilobytes per second each way for each relayed fight, an estimate).
- A home server shows your home's address to everyone, and an attack on the server takes your home connection down with it; a VPS avoids both.
- A known limit: any signed-in player can find the relayed match a fight falls back to and join it. They can neither read nor change the fight, but they can flood the two players with data of their own and spoil that fight.

For your players, [Security and privacy](README.md#security-and-privacy) says what the game protects and what it shows to whom; it is worth telling them.

## Troubleshooting

| What players see | Where to look |
|---|---|
| "Could not reach the online service." | `https://<name>/healthcheck` from outside your network; the router's forward of TCP 443; that the name points at your current address; the profile's `host`, `port` 443 and `ssl` true. |
| A certificate error | The server computer's clock; Caddy's log (Let's Encrypt limits how often a name gets certificates); `key_type rsa2048` for older systems. |
| Searching forever in Quick Match | Both copies must have the same `game`, `build` and `region`; a lobby refuses a copy of another game or build. |
| "The other player's game is an older version." | One player has an older copy of the game: both need the latest download, and a new `build` keeps versions apart. |
| "Could not open a connection to the other player, either directly or through the server." | Rare: neither a direct way nor the relay through the server worked within 30 seconds. Both games' logs name each way they tried. |
