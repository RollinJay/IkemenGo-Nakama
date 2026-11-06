# Server kit

Ready-made files for running your game's online server on a Linux PC or a
Linux VPS, the way [hosting.md](../hosting.md) describes it: the scripts, the
systemd services, the HTTPS setup and a window with buttons for all of it.
They come from the setup that runs a real community (Hyper DBZ).

Copy this folder somewhere of your own, such as `~/mygame-server`, and keep
that copy private: once running, it holds the server's keys, its database and
your DuckDNS token. Its `.gitignore` keeps them out of git.

Linux only: the services are systemd's, and the window needs GTK 4 and
libadwaita. On Windows, the fork's own launcher is the way for now
([Optional dedicated-server launcher](../README.md#optional-dedicated-server-launcher)).

| File | What it does |
|---|---|
| `settings.env.example` | copy it to `settings.env`: the server's id, the game folder, the game id, build and region, where the programs are |
| `server.sh` | runs Nakama Matchmaker's `nakama-matchmaker-server` with this folder's `config/` |
| `set-public-host.sh` | sets the public name in the game folder's `data/online/servers.json`, in `host-profile.json`, in the server and in Caddy |
| `run-caddy.sh`, `caddy/Caddyfile` | HTTPS in front of the server, with a free Let's Encrypt certificate |
| `ddns/duckdns-update.sh` | points a DuckDNS name at your home's address (with your name and token in `ddns/duckdns.env`) |
| `install-services.sh` | installs the services `<id>-server`, `<id>-caddy` and `<id>-duckdns`, and the window in the app menu; `--remove` removes them |
| `server-control.py` | the window: the services, the public name, build and DuckDNS, playing on this PC, the logs |
| `play-on-this-pc.sh` | starts the game signed in to the server directly (127.0.0.1:7350), with `host-profile.json` |
| `package-game-zip.py` | builds the zip you share, without your own account and other per-PC files; `--android <apk>` builds the Android one |
| `settings.sh` | how the scripts read `settings.env` |

## Setting it up

Run everything as an ordinary user, not root.

1. Choose where it runs, give it a name and open one port: [hosting.md](../hosting.md),
   steps 1, 4 and 5. At home, forward the router's TCP 443 to this PC's
   port 8443, where Caddy listens.
2. Copy this folder, and work in the copy:
   ```bash
   cp -r nakama/server-kit ~/mygame-server
   ```
   ```bash
   cd ~/mygame-server
   ```
3. Get the programs ([hosting.md](../hosting.md), step 2): Nakama, PostgreSQL,
   Nakama Matchmaker's server program and Caddy. Put the last two in
   `programs/` here (`programs/matchmaker/bin/nakama-matchmaker-server`,
   `programs/caddy/caddy`), or name them in `settings.env` (`MATCHMAKER=`,
   `CADDY=`).
4. Make `settings.env` from `settings.env.example`, and fill it in:
   ```bash
   cp settings.env.example settings.env
   ```
5. Tell the server program where Nakama is, then create the server, with
   your `SERVER_ID` as its id. Keep Nakama's port 7350: the Caddyfile and
   `set-public-host.sh` use it.
   ```bash
   ./server.sh programs --nakama ~/server/nakama/nakama
   ```
   ```bash
   ./server.sh add mygame --kinds ikemen --title "My Game Online" --bind 127.0.0.1 --public-port 443 --client-tls yes
   ```
6. Install the services and the window, and start the server:
   ```bash
   ./install-services.sh
   ```
   To keep them running while you are logged out (some systems ask for an
   administrator's password):
   ```bash
   loginctl enable-linger $USER
   ```
7. Give the server its public name: on the window's **Public name** page, or
   ```bash
   ./set-public-host.sh mygame.example.org
   ```
   It writes the game folder's `data/online/servers.json` and starts Caddy,
   which gets the certificate.
8. With a DuckDNS name, save its name and token on the same page (or in
   `ddns/duckdns.env`, readable by you alone; see `ddns/duckdns-update.sh`).
9. Test it ([hosting.md](../hosting.md), step 9), then build the zip you
   share:
   ```bash
   python3 package-game-zip.py
   ```

## The window

`server-control.py` opens from the app menu under your `TITLE` once
`install-services.sh` has run, or with `python3 server-control.py`. It needs
Python 3 with PyGObject, GTK 4 and libadwaita 1.5 or newer; on Debian and
Ubuntu, `sudo apt install python3-gi gir1.2-gtk-4.0 gir1.2-adw-1`.

- **Server**: whether the server, Caddy and the DuckDNS updater run and
  answer, with start, stop and restart buttons, and whether they start with
  this PC; what players' games use; Nakama's console (**Password** copies its
  password, **Open** opens it in the browser).
- **Public name**: the public name, build, game id, region and game folder.
  **Apply** saves them in `settings.env` and runs `set-public-host.sh`. The
  DuckDNS name and token go into `ddns/duckdns.env`; the token is never shown.
- **Play on this PC**: runs `play-on-this-pc.sh`. Many home routers cannot
  reach their own public address from inside the network, so the server's PC
  plays through 127.0.0.1 instead.
- **Logs**: the server's, Nakama's, Caddy's and DuckDNS's, with passwords and
  tokens shown as dots.

Closing the window leaves the server running. A change on the Public name page
reaches players only in a new copy of the game: build and share the zip again.

## Day to day

- **A new release** that cannot play the one before it: change the build on
  the Public name page and Apply (or `BUILD=` in `settings.env`, then
  `./set-public-host.sh` again), then build and share a new zip. Keep the game
  id: ratings and leaderboards are kept per game id.
- **Logs**: the window's Logs page, `journalctl --user -u mygame-server`, or
  `config/servers/mygame/server.log`.
- **Moderation**: the console (the window's Open and Password buttons, or
  `./server.sh show mygame --secrets`), and the ranked sets:
  `./server.sh ranked mygame` ([hosting.md](../hosting.md), step 11).
- **Backups**: stop the services, then keep an archive of this folder that
  keeps its permissions (PostgreSQL refuses a database folder others can
  read): `tar -C ~ -czf ~/mygame-backup.tgz mygame-server`. The archive is
  as private as the folder.
- **Security**: [hosting.md](../hosting.md), step 12.
