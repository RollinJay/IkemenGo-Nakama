#!/usr/bin/env python3
"""A window for running IKEMEN GO games' online servers with this kit.

One kit folder runs one server or several: for the same game (other builds,
regions or communities) or for other games. Each has servers/<id>.env; the
server chosen at the top of the window is the one the pages show and change.
The window does with buttons what the files next to it do:
- the <id>-server services, one per server, and the <kit>-caddy and
  <kit>-duckdns services all the servers share (systemctl --user;
  install-services.sh installs them): start, stop, restart, and whether they
  start with this PC;
- add-server.sh and remove-server.sh: a server more or less;
- set-public-host.sh: a server's public name, and the game id, build and
  region players are paired by (kept in servers/<id>.env, which the scripts
  read);
- ddns/duckdns.env: the DuckDNS token and names;
- play-on-this-pc.sh with host-profiles/<id>.json: the game on this PC,
  signed in to a server directly;
- the logs.

    python3 server-control.py

It needs Python 3 with PyGObject, GTK 4 and libadwaita 1.5 or newer (Debian
and Ubuntu: python3-gi gir1.2-gtk-4.0 gir1.2-adw-1), and Linux with systemd.
Tests point it at another kit folder with SERVER_KIT_DIR, replace systemctl
and journalctl with SERVER_KIT_SYSTEMCTL and SERVER_KIT_JOURNALCTL, and open a
page with SERVER_KIT_PAGE (server, public, play or logs).
"""
import glob
import http.client
import json
import os
import re
import shlex
import socket
import ssl
import stat
import subprocess
import tempfile
import threading
import time

import gi

gi.require_version("Gtk", "4.0")
gi.require_version("Adw", "1")
from gi.repository import Adw, Gdk, Gio, GLib, GObject, Gtk  # noqa: E402

HERE = os.path.realpath(os.environ.get("SERVER_KIT_DIR")
                        or os.path.dirname(os.path.realpath(__file__)))
SYSTEMCTL = shlex.split(os.environ.get("SERVER_KIT_SYSTEMCTL", "systemctl")) + ["--user"]
JOURNALCTL = shlex.split(os.environ.get("SERVER_KIT_JOURNALCTL", "journalctl")) + ["--user"]

SETTINGS = os.path.join(HERE, "settings.env")
SERVERS_DIR = os.path.join(HERE, "servers")
HOST_PROFILES_DIR = os.path.join(HERE, "host-profiles")
CADDYFILE = os.path.join(HERE, "caddy", "Caddyfile")
SITES_DIR = os.path.join(HERE, "caddy", "sites")
DUCKDNS_ENV = os.path.join(HERE, "ddns", "duckdns.env")
MATCHMAKER_SERVERS = os.path.join(HERE, "config", "servers.json")
UNITS_DIR = (os.environ.get("SYSTEMD_USER_DIR")
             or os.path.join(os.path.expanduser("~"), ".config", "systemd", "user"))
SERVER_SH = os.path.join(HERE, "server.sh")
SET_PUBLIC_HOST_SH = os.path.join(HERE, "set-public-host.sh")
ADD_SERVER_SH = os.path.join(HERE, "add-server.sh")
REMOVE_SERVER_SH = os.path.join(HERE, "remove-server.sh")
UPGRADE_KIT_SH = os.path.join(HERE, "upgrade-kit.sh")
PLAY_SH = os.path.join(HERE, "play-on-this-pc.sh")

# The names set-public-host.sh accepts: lowercase, at least one dot, no IP.
HOST_RE = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$")
# A server's id, as the scripts accept it; it names its service and files.
ID_RE = re.compile(r"^[a-z0-9][a-z0-9-]*$")
WORD_RE = re.compile(r"^[A-Za-z0-9._-]{1,64}$")
DUCKDNS_RE = re.compile(r"^[A-Za-z0-9.-]{1,128}$")
LOG_LINES = 400
# Titles and descriptions of rows and groups are Pango markup: & and < in a
# server's title or a folder's name are escaped first.
esc = GLib.markup_escape_text

# Logs show passwords, tokens and keys as dots, in case one ever reaches them.
SECRET_PATTERNS = (
    (re.compile(r"(://[^/\s:@]+:)[^@\s]+@"), r"\1•••@"),
    (re.compile(r"(?i)((?:token|password|passwd|secret|http_key|server_key|signing_key"
                r"|encryption_key)\W{0,3}[=:]\W{0,3})[^\s&\"',}]+"), r"\1•••"),
)


# Files ----------------------------------------------------------------------

def read_env(path):
    """KEY=value lines, as the scripts read them (the last one wins)."""
    values = {}
    try:
        with open(path, encoding="utf-8") as f:
            for line in f:
                line = line.rstrip("\r\n")
                if "=" in line and not line.lstrip().startswith("#"):
                    key, value = line.split("=", 1)
                    values[key.strip()] = value
    except OSError:
        pass
    return values


def unquote(value):
    """A value as duckdns-update.sh reads it: no spaces or quotes around it."""
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
        value = value[1:-1]
    return value


def write_file(path, text, mode):
    """Replaces a file in one step, with exactly this mode. The new file is
    made private first (mkstemp), so a secret is never readable by others."""
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".server-control-")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def update_env(path, updates):
    """Sets KEY=value lines in a settings file, keeping its other lines."""
    try:
        with open(path, encoding="utf-8") as f:
            lines = f.read().splitlines()
    except FileNotFoundError:
        lines = []
    out, done = [], set()
    for line in lines:
        key = (line.split("=", 1)[0].strip()
               if "=" in line and not line.lstrip().startswith("#") else None)
        if key in updates:
            if key not in done:  # one line per key: the scripts take the last
                out.append(f"{key}={updates[key]}")
                done.add(key)
        else:
            out.append(line)
    out += [f"{key}={value}" for key, value in updates.items() if key not in done]
    mode = stat.S_IMODE(os.stat(path).st_mode) if os.path.exists(path) else 0o644
    write_file(path, "\n".join(out) + "\n", mode)


def load_profiles(path):
    """An IKEMEN GO server profile file ({"default", "servers"}), or None."""
    try:
        with open(path, encoding="utf-8") as f:
            profiles = json.load(f)
        return profiles if isinstance(profiles.get("servers"), list) else None
    except (OSError, ValueError, AttributeError):
        return None


def find_profile(path, server_id=None):
    """The profile for this server in a profile file (ikemen-profile names it
    after the server's id), or the file's default one; None without either."""
    profiles = load_profiles(path)
    if not profiles:
        return None
    wanted = server_id if server_id is not None else profiles.get("default")
    for server in profiles["servers"]:
        if isinstance(server, dict) and server.get("id") == wanted:
            return server
    return None


def shared_profile_path(game_dir):
    return os.path.join(game_dir, "data", "online", "servers.json")


def kit_settings():
    return read_env(SETTINGS)


def kit_id():
    """KIT_ID names the services all the servers share: <kit>-caddy and
    <kit>-duckdns. add-server.sh sets it to the first server's id."""
    kid = kit_settings().get("KIT_ID", "")
    if kid:
        # As settings.sh: anything else is no id (the scripts refuse it).
        return kid if ID_RE.match(kid) else ""
    ids = server_ids()
    return ids[0] if ids else ""


def kit_title():
    kid = kit_id()
    return (kit_settings().get("TITLE", "").strip()
            or (f"{kid} servers" if kid else "Game servers"))


def server_ids():
    """The servers of this kit: servers/<id>.env, sorted."""
    ids = []
    for path in glob.glob(os.path.join(SERVERS_DIR, "*.env")):
        name = os.path.basename(path)[:-len(".env")]
        if ID_RE.match(name):
            ids.append(name)
    return sorted(ids)


def legacy_layout():
    """A kit from before servers/: settings.env names its one server. As in
    settings.sh: upgrade-kit.sh comments SERVER_ID out last, so an upgrade
    that stopped halfway (servers/ made already) goes on too."""
    return bool(kit_settings().get("SERVER_ID", "").strip())


def app_id_for(kid):
    """As install-services.sh names the launcher."""
    part = re.sub(r"[^A-Za-z0-9]", "_", kid) or "unset"
    return "local.IkemenServer." + ("s_" + part if part[0].isdigit() else part)


def matchmaker_servers():
    """The servers in config/servers.json, by id: their ports, console user and
    folder. The file holds their keys too; only these fields are kept."""
    try:
        with open(MATCHMAKER_SERVERS, encoding="utf-8") as f:
            servers = json.load(f).get("servers", [])
    except (OSError, ValueError, AttributeError):
        return {}
    found = {}
    for s in servers if isinstance(servers, list) else []:
        if isinstance(s, dict) and s.get("id"):
            found[s["id"]] = {key: s.get(key) for key in
                              ("port", "console_port", "console_user", "data_dir", "title")}
    return found


def site_files():
    return sorted(glob.glob(os.path.join(SITES_DIR, "*.caddy")))


def unit_elsewhere(unit):
    """As settings.sh: a unit file of that name that runs a program outside
    this folder (a timer: its service does), or no name at all (a KIT_ID
    that is no id). Every folder of the same user shares unit names."""
    if unit.startswith("-"):
        return True
    if unit.endswith(".timer"):
        unit = unit[:-len(".timer")] + ".service"
    try:
        with open(os.path.join(UNITS_DIR, unit), encoding="utf-8") as f:
            start = next((line[len("ExecStart="):] for line in f
                          if line.startswith("ExecStart=")), "")
    except OSError:
        return False
    return not start.startswith(HERE + "/")


def caddy_unit():
    return f"{kit_id()}-caddy.service"


def duckdns_unit():
    return f"{kit_id()}-duckdns.service"


def duckdns_timer():
    return f"{kit_id()}-duckdns.timer"


def duckdns_label(host):
    """The DuckDNS name in a public name (game.duckdns.org: game), or ""."""
    if host.endswith(".duckdns.org"):
        return host[:-len(".duckdns.org")].rsplit(".", 1)[-1]
    return ""


class Server:
    """One server of the kit: servers/<id>.env, its service, its files, and
    its ports in config/servers.json."""

    def __init__(self, server_id):
        self.id = server_id
        self.env_path = os.path.join(SERVERS_DIR, server_id + ".env")
        self.unit = f"{server_id}-server.service"
        self.host_profile = os.path.join(HOST_PROFILES_DIR, server_id + ".json")
        self.load()

    def load(self):
        env = read_env(self.env_path)
        self.saved = env
        self.title = env.get("TITLE", "").strip() or self.id
        self.game_dir = env.get("GAME_DIR", "")
        self.public_host = env.get("PUBLIC_HOST", "").strip()
        mm = matchmaker_servers().get(self.id, {})
        self.port = mm.get("port") or None
        self.console_port = mm.get("console_port") or None
        self.console_user = mm.get("console_user") or None
        folder = mm.get("data_dir") or os.path.join("servers", self.id)
        if not os.path.isabs(folder):
            folder = os.path.join(HERE, "config", folder)
        self.nakama_log = os.path.join(folder, "server.log")

    def settings(self):
        """The game folder, game id, build and region in use: servers/<id>.env's,
        else those the game folder's profile was made with, else the scripts'."""
        shared = find_profile(shared_profile_path(self.game_dir), self.id) or {}
        return {
            "GAME_DIR": self.game_dir,
            "GAME": self.saved.get("GAME") or shared.get("game") or self.id,
            "BUILD": self.saved.get("BUILD") or shared.get("build") or "",
            "REGION": self.saved.get("REGION") or shared.get("region") or "us-west",
        }

    def shared_profile(self):
        return find_profile(shared_profile_path(self.game_dir), self.id)

    def has_site(self):
        return os.path.exists(os.path.join(SITES_DIR, self.id + ".caddy"))


def caddy_https_port():
    try:
        with open(CADDYFILE, encoding="utf-8") as f:
            m = re.search(r"^\s*https_port\s+(\d+)", f.read(), re.M)
        return int(m.group(1)) if m else 443
    except OSError:
        return 443


def mask(text):
    for pattern, replacement in SECRET_PATTERNS:
        text = pattern.sub(replacement, text)
    return text


def tail(path, lines):
    try:
        with open(path, "rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            f.seek(max(0, size - 512 * 1024))
            data = f.read().decode("utf-8", "replace")
    except OSError as e:
        return f"Cannot read {path}: {e.strerror}"
    return "\n".join(data.splitlines()[-lines:])


# Commands -------------------------------------------------------------------

def run(argv, timeout=60):
    """Runs a command and returns (exit code, output); never raises."""
    try:
        p = subprocess.run(argv, capture_output=True, text=True, timeout=timeout,
                           stdin=subprocess.DEVNULL, cwd=HERE)
        return p.returncode, (p.stdout + p.stderr).strip()
    except subprocess.TimeoutExpired:
        return -1, f"{os.path.basename(argv[0])} did not finish in {timeout} s"
    except OSError as e:
        return -1, f"{argv[0]}: {e.strerror}"


def unit_states(names):
    """systemctl show for these units: {unit: {property: value}}."""
    props = ("Id,LoadState,ActiveState,SubState,Result,ActiveEnterTimestamp,ExecMainExitTimestamp,"
             "ExecMainStatus,UnitFileState,LastTriggerUSec")
    code, out = run(SYSTEMCTL + ["show", "--timestamp=unix", "-p", props, *names], timeout=15)
    units = {}
    for block in out.split("\n\n"):
        values = dict(line.split("=", 1) for line in block.splitlines() if "=" in line)
        if "Id" in values:
            units[values["Id"]] = values
    return units, (None if code == 0 else out)


def check_nakama(port):
    try:
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
        c.request("GET", "/healthcheck")
        status = c.getresponse().status
        c.close()
        return status == 200, f"HTTP {status}"
    except OSError as e:
        return False, e.strerror or str(e)


def check_https(host, port):
    """Asks Caddy on this PC for a server's health under its public name,
    checking its certificate for that name as players' games do."""
    try:
        context = ssl.create_default_context()
        with socket.create_connection(("127.0.0.1", port), timeout=4) as raw:
            with context.wrap_socket(raw, server_hostname=host) as s:
                s.settimeout(4)
                s.sendall(f"GET /healthcheck HTTP/1.1\r\nHost: {host}\r\n"
                          "Connection: close\r\n\r\n".encode())
                answer = s.recv(256).decode("latin-1")
        status = answer.split(" ", 2)[1] if answer.startswith("HTTP/") else "?"
        return status == "200", f"HTTP {status}"
    except ssl.SSLCertVerificationError as e:
        return False, f"certificate: {e.verify_message}"
    except (OSError, IndexError) as e:
        return False, getattr(e, "strerror", None) or str(e)


def when(timestamp):
    """A systemd --timestamp=unix value ("@1762215574") for people."""
    try:
        seconds = int(timestamp.strip().lstrip("@").split()[0])
    except (ValueError, IndexError):
        return ""
    if seconds <= 0:
        return ""
    t, now = time.localtime(seconds), time.localtime()
    clock = time.strftime("%-I:%M %p", t)
    if (t.tm_year, t.tm_yday) == (now.tm_year, now.tm_yday):
        return clock
    return time.strftime("%a %b %-d, ", t) + clock


def describe(unit):
    """A unit's state for people: (ok, warning, error or off; words)."""
    active = unit.get("ActiveState", "")
    since = when(unit.get("ActiveEnterTimestamp", ""))
    if active == "active":
        return "ok", "Running" + (f" since {since}" if since else "")
    if active in ("activating", "reloading"):
        return "warning", "Starting…"
    if active == "deactivating":
        return "warning", "Stopping…"
    if active == "failed":
        return "error", f"Failed ({unit.get('Result', '?')}): see Logs"
    if unit.get("LoadState") == "not-found":
        return "off", "Not installed"
    if not active:
        return "error", "Unknown: systemctl did not report it"
    return "off", "Stopped"


def running(unit):
    return unit.get("ActiveState") in ("active", "activating", "reloading")


def in_background(work, done):
    """Runs work() in a thread, then done(its result) in the window's thread."""
    def finish(result):
        done(result)
        return False

    def thread():
        GLib.idle_add(finish, work())
    threading.Thread(target=thread, daemon=True).start()


# Widgets --------------------------------------------------------------------

STATE_ICONS = {"ok": "object-select-symbolic", "warning": "dialog-warning-symbolic",
               "error": "dialog-error-symbolic", "off": "media-playback-stop-symbolic"}
STATE_CSS = {"ok": "success", "warning": "warning", "error": "error", "off": "dim-label"}


def show_state(icon, state):
    icon.set_from_icon_name(STATE_ICONS[state])
    for css in STATE_CSS.values():
        icon.remove_css_class(css)
    icon.add_css_class(STATE_CSS[state])


def icon_button(icon, tooltip, callback, label=None):
    button = Gtk.Button(valign=Gtk.Align.CENTER, tooltip_text=tooltip)
    if label:
        button.set_child(Adw.ButtonContent(icon_name=icon, label=label))
    else:
        button.set_icon_name(icon)
        button.add_css_class("flat")
        button.update_property([Gtk.AccessibleProperty.LABEL], [tooltip])
    button.connect("clicked", lambda _b: callback())
    return button


class ServiceRow(Adw.ActionRow):
    """A service's state, with start, stop and restart buttons."""

    def __init__(self, title, on_start, on_stop, on_restart):
        super().__init__(title=title, subtitle="…")
        self.icon = Gtk.Image(icon_name="content-loading-symbolic")
        self.add_prefix(self.icon)
        self.start = icon_button("media-playback-start-symbolic", "Start", on_start)
        self.stop = icon_button("media-playback-stop-symbolic", "Stop", on_stop)
        self.restart = icon_button("view-refresh-symbolic", "Restart", on_restart)
        for button in (self.start, self.stop, self.restart):
            self.add_suffix(button)

    def show(self, state, text, is_running):
        """state: ok, warning, error or off."""
        show_state(self.icon, state)
        self.set_subtitle(text)
        self.start.set_visible(not is_running)
        self.stop.set_visible(is_running)
        self.restart.set_visible(is_running)

    def set_busy(self, busy):
        for button in (self.start, self.stop, self.restart):
            button.set_sensitive(not busy)


def info_row(title, subtitle="", copy_text=None, window=None):
    row = Adw.ActionRow(title=title, subtitle=subtitle)
    row.add_css_class("property")
    row.set_subtitle_selectable(True)
    if copy_text is not None:
        row.copy_button = icon_button("edit-copy-symbolic", f"Copy the {title.lower()}",
                                      lambda: window.copy(row.copy_value, f"{title} copied"))
        row.copy_value = copy_text
        row.add_suffix(row.copy_button)
    return row


class Dialogs:
    """What both windows show: toasts, questions, command output, and the
    dialog that adds a server. The window has self.toasts (Adw.ToastOverlay)."""

    def toast(self, text):
        toast = Adw.Toast.new(text)
        toast.set_timeout(4)
        self.toasts.add_toast(toast)

    def confirm(self, heading, body, action, on_yes, destructive=False):
        dialog = Adw.AlertDialog.new(heading, body)
        dialog.add_response("cancel", "Cancel")
        dialog.add_response("ok", action)
        dialog.set_response_appearance(
            "ok", Adw.ResponseAppearance.DESTRUCTIVE if destructive
            else Adw.ResponseAppearance.SUGGESTED)
        dialog.set_default_response("cancel" if destructive else "ok")
        dialog.set_close_response("cancel")
        dialog.connect("response", lambda _d, response: on_yes() if response == "ok" else None)
        dialog.present(self)

    def report(self, heading, text):
        """A dialog with a command's output."""
        dialog = Adw.AlertDialog.new(heading, None)
        view = Gtk.TextView(editable=False, monospace=True, wrap_mode=Gtk.WrapMode.WORD_CHAR,
                            top_margin=8, bottom_margin=8, left_margin=8, right_margin=8)
        view.get_buffer().set_text(mask(text) or "(no output)")
        scroll = Gtk.ScrolledWindow(child=view, min_content_height=160,
                                    max_content_height=360, propagate_natural_height=True)
        scroll.add_css_class("card")
        dialog.set_extra_child(scroll)
        dialog.add_response("close", "Close")
        dialog.present(self)

    def ask_add_server(self, on_added, game_dir=""):
        """Asks for a new server's id and settings, then runs add-server.sh;
        on_added(id) once it has worked."""
        dialog = Adw.AlertDialog.new(
            "Add a server",
            "A server of its own: its own accounts, matches and port. For another "
            "game, or for the same game with another build, region or community. The "
            "public name comes after, on the Public name page.")
        rows = Gtk.ListBox(selection_mode=Gtk.SelectionMode.NONE)
        rows.add_css_class("boxed-list")
        id_entry = Adw.EntryRow(title="Id: lowercase letters, digits and dashes, such as mygame")
        title_entry = Adw.EntryRow(title="Title, such as My Game Online")
        game_entry = Adw.EntryRow(title="Game id (empty: the id)")
        build_entry = Adw.EntryRow(title="Build, such as mygame-1.0")
        region_entry = Adw.EntryRow(title="Region (empty: us-west)")
        folder = {"path": game_dir}
        folder_row = Adw.ActionRow(title="Game folder",
                                   subtitle=esc(game_dir) if game_dir else "Not chosen yet")
        folder_row.set_subtitle_selectable(True)

        def choose():
            chooser = Gtk.FileDialog(title="The game folder you share")
            if os.path.isdir(folder["path"]):
                chooser.set_initial_folder(Gio.File.new_for_path(folder["path"]))

            def chosen(_chooser, result):
                try:
                    picked = chooser.select_folder_finish(result)
                except GLib.Error:
                    return  # cancelled
                path = picked.get_path() if picked else None
                if path and "\n" not in path and "\r" not in path:
                    folder["path"] = path
                    folder_row.set_subtitle(esc(path))
            chooser.select_folder(self, None, chosen)
        folder_row.add_suffix(icon_button("folder-open-symbolic", "Choose the game folder",
                                          choose, "Choose…"))
        for row in (id_entry, title_entry, folder_row, game_entry, build_entry, region_entry):
            rows.append(row)
        dialog.set_extra_child(rows)
        dialog.add_response("cancel", "Cancel")
        dialog.add_response("add", "Add")
        dialog.set_response_appearance("add", Adw.ResponseAppearance.SUGGESTED)
        dialog.set_default_response("add")
        dialog.set_close_response("cancel")
        taken = set(server_ids())

        def check(*_):
            sid = id_entry.get_text().strip()
            words = [e.get_text().strip() for e in (game_entry, build_entry, region_entry)]
            ok = (bool(ID_RE.match(sid)) and sid not in taken
                  and all(not w or WORD_RE.match(w) for w in words)
                  and "\n" not in title_entry.get_text())
            dialog.set_response_enabled("add", ok)
            if sid and not ID_RE.match(sid):
                id_entry.add_css_class("error")
            elif sid in taken:
                id_entry.add_css_class("error")
            else:
                id_entry.remove_css_class("error")
        for entry in (id_entry, title_entry, game_entry, build_entry, region_entry):
            entry.connect("changed", check)
        check()

        def response(_dialog, answer):
            if answer != "add":
                return
            sid = id_entry.get_text().strip()
            argv = [ADD_SERVER_SH, sid]
            for flag, value in (("--title", title_entry.get_text().strip()),
                                ("--game-dir", folder["path"]),
                                ("--game", game_entry.get_text().strip()),
                                ("--build", build_entry.get_text().strip()),
                                ("--region", region_entry.get_text().strip())):
                if value:
                    argv += [flag, value]
            self.toast(f"Adding {sid}…")

            def done(result):
                code, out = result
                if code != 0:
                    self.report(f"Could not add the server {sid}", out)
                else:
                    self.toast(f"Added the server {sid}")
                    on_added(sid)
            # Its first start makes its database: give the program the time.
            in_background(lambda: run(argv, timeout=300), done)
        dialog.connect("response", response)
        dialog.present(self)


# The window -----------------------------------------------------------------

class Window(Dialogs, Adw.ApplicationWindow):
    def __init__(self, app, select=None):
        super().__init__(application=app, title=kit_title(), default_width=780,
                         default_height=760)
        self.set_size_request(360, 400)
        self.refreshing = False
        self.refresh_again = False
        self.units = {}
        self.server_running = False
        self.server = None
        self.ids = []
        self.server_rows = {}
        self.choice_syncing = False

        self.toasts = Adw.ToastOverlay()
        self.stack = Adw.ViewStack()
        self.toasts.set_child(self.stack)
        header = Adw.HeaderBar()
        switcher = Adw.ViewSwitcher(stack=self.stack, policy=Adw.ViewSwitcherPolicy.WIDE)
        header.set_title_widget(switcher)
        # Which server the pages are for.
        self.server_names = Gtk.StringList()
        self.server_choice = Gtk.DropDown(model=self.server_names, valign=Gtk.Align.CENTER,
                                          tooltip_text="The server these pages are for")
        self.server_choice.update_property([Gtk.AccessibleProperty.LABEL], ["Server"])
        self.server_choice.connect("notify::selected", lambda *_: self.server_chosen())
        header.pack_start(self.server_choice)
        menu = Gio.Menu()
        menu.append("Add server…", "win.add-server")
        menu.append("Remove server…", "win.remove-server")
        header.pack_end(Gtk.MenuButton(icon_name="open-menu-symbolic", menu_model=menu,
                                       primary=True, tooltip_text="Servers"))
        for name, callback in (("add-server", self.add_server),
                               ("remove-server", self.ask_remove_server)):
            action = Gio.SimpleAction.new(name, None)
            action.connect("activate", lambda *_a, c=callback: c())
            self.add_action(action)
        bottom = Adw.ViewSwitcherBar(stack=self.stack)
        view = Adw.ToolbarView()
        view.add_top_bar(header)
        view.set_content(self.toasts)
        view.add_bottom_bar(bottom)
        self.set_content(view)
        # Narrow windows move the page buttons to the bottom.
        narrow = Adw.Breakpoint.new(Adw.BreakpointCondition.parse("max-width: 560sp"))
        no_widget = GObject.Value(Gtk.Widget)
        no_widget.set_object(None)
        narrow.add_setter(header, "title-widget", no_widget)
        narrow.add_setter(bottom, "reveal", True)
        self.add_breakpoint(narrow)

        self.stack.add_titled_with_icon(self.server_page(), "server", "Server",
                                        "network-server-symbolic")
        self.stack.add_titled_with_icon(self.public_page(), "public", "Public name",
                                        "network-workgroup-symbolic")
        self.stack.add_titled_with_icon(self.play_page(), "play", "Play on this PC",
                                        "input-gaming-symbolic")
        self.stack.add_titled_with_icon(self.logs_page(), "logs", "Logs",
                                        "utilities-terminal-symbolic")
        page = os.environ.get("SERVER_KIT_PAGE")
        if page:
            self.stack.set_visible_child_name(page)
        self.stack.connect("notify::visible-child-name", lambda *_: self.page_changed())

        self.reload_servers(select)
        GLib.timeout_add_seconds(4, self.tick)

    # Helpers ---------------------------------------------------------------

    def copy(self, text, what):
        Gdk.Display.get_default().get_clipboard().set(text)
        self.toast(what)

    def command(self, argv, done, busy=None, timeout=120):
        """Runs a command in the background; done(code, output) afterwards."""
        if busy:
            busy(True)

        def finished(result):
            if busy:
                busy(False)
            done(*result)
            self.refresh()
        in_background(lambda: run(argv, timeout), finished)

    # The servers -------------------------------------------------------------

    def reload_servers(self, select=None):
        """Reads servers/ again: the server list, the switcher, and the server
        the pages show (select, else the one shown, else the first)."""
        ids = server_ids()
        if not ids:
            self.get_application().show_setup()
            self.close()
            return
        current = self.server.id if self.server else None
        wanted = select if select in ids else current if current in ids else ids[0]
        servers = {sid: Server(sid) for sid in ids}
        titles = [servers[sid].title for sid in ids]
        names = [t if titles.count(t) == 1 and t != sid else
                 (sid if t == sid else f"{t} ({sid})") for sid, t in zip(ids, titles)]
        self.ids = ids
        self.choice_syncing = True
        self.server_names.splice(0, self.server_names.get_n_items(), names)
        self.server_choice.set_selected(ids.index(wanted))
        self.choice_syncing = False
        self.rebuild_server_rows(servers)
        if wanted != current:
            self.select_server(wanted)
        else:
            self.server.load()
            self.show_server()

    def server_chosen(self):
        if self.choice_syncing:
            return
        index = self.server_choice.get_selected()
        if 0 <= index < len(self.ids) and (not self.server or self.ids[index] != self.server.id):
            self.select_server(self.ids[index])

    def select_server(self, sid):
        self.server = Server(sid)
        if sid in self.ids and self.server_choice.get_selected() != self.ids.index(sid):
            self.choice_syncing = True
            self.server_choice.set_selected(self.ids.index(sid))
            self.choice_syncing = False
        for row_id, (_row, _icon, check) in self.server_rows.items():
            check.set_visible(row_id == sid)
        self.show_server()
        self.load_settings()
        self.load_console()
        if self.stack.get_visible_child_name() == "logs":
            self.log_view.get_buffer().set_text("")
            self.load_log()
        self.refresh()

    def show_server(self):
        """The selected server's names on the pages."""
        s = self.server
        self.this_server.set_title(esc(s.title))
        self.this_server.set_description(
            f"Server {s.id}: Nakama and its database"
            + (f", on port {s.port}" if s.port else "")
            + ". It runs in the background, also with this window closed.")
        self.name_group.set_title(esc(f"Public name of {s.title}"))
        self.profile_row.set_title(f"This PC's profile (host-profiles/{s.id}.json)")

    def rebuild_server_rows(self, servers):
        if list(self.server_rows) == list(servers) and all(
                self.server_rows[sid][0].get_title() == esc(s.title) for sid, s in servers.items()):
            return
        for row, _icon, _check in self.server_rows.values():
            self.all_servers.remove(row)
        self.server_rows = {}
        for sid, s in servers.items():
            row = Adw.ActionRow(title=esc(s.title), subtitle=sid, activatable=True)
            icon = Gtk.Image(icon_name="content-loading-symbolic")
            row.add_prefix(icon)
            check = Gtk.Image(icon_name="object-select-symbolic",
                              tooltip_text="Shown on these pages")
            check.set_visible(self.server is not None and sid == self.server.id)
            row.add_suffix(check)
            row.connect("activated", lambda _r, i=sid: self.select_server(i))
            self.all_servers.add(row)
            self.server_rows[sid] = (row, icon, check)

    def add_server(self):
        self.ask_add_server(lambda sid: self.reload_servers(sid),
                            self.server.game_dir if self.server else "")

    def ask_remove_server(self):
        s = self.server
        if not s:
            return
        dialog = Adw.AlertDialog.new(
            f"Remove the server {s.title}?",
            f"This stops {s.id} and removes its service, its public name in Caddy, its "
            "profile in the game folder's data/online/servers.json and this PC's profile, "
            "and the server itself from config/. Players cannot sign in to it anymore. "
            "Its database stays in its folder unless you delete it too.")
        delete = Gtk.CheckButton(label="Also delete its folder: its database (accounts, "
                                       "ratings), logs and settings. This cannot be undone.")
        dialog.set_extra_child(delete)
        dialog.add_response("cancel", "Cancel")
        dialog.add_response("remove", "Remove")
        dialog.set_response_appearance("remove", Adw.ResponseAppearance.DESTRUCTIVE)
        dialog.set_default_response("cancel")
        dialog.set_close_response("cancel")

        def response(_dialog, answer):
            if answer != "remove":
                return
            argv = [REMOVE_SERVER_SH, s.id] + (["--delete-data"] if delete.get_active() else [])

            def done(code, out):
                if code != 0:
                    self.report(f"Could not remove the server {s.id}", out)
                else:
                    self.toast(f"Removed the server {s.id}")
                self.reload_servers()
            self.toast(f"Removing {s.id}…")
            self.command(argv, done, timeout=180)
        dialog.connect("response", response)
        dialog.present(self)

    # Server page -------------------------------------------------------------

    def server_page(self):
        page = Adw.PreferencesPage()
        self.banner = Adw.Banner(revealed=False)
        self.this_server = Adw.PreferencesGroup(title="…")
        self.server_row = ServiceRow(
            "Game server",
            lambda: self.service("start", self.server.unit, self.server_row),
            lambda: self.confirm(
                f"Stop {self.server.title}?",
                "Players online now are disconnected, and nobody can sign in or find "
                "matches until you start it again.",
                "Stop", lambda: self.service("stop", self.server.unit, self.server_row), True),
            lambda: self.confirm(
                f"Restart {self.server.title}?",
                "Players online now are disconnected for a few seconds. Matches between "
                "players connected directly go on; matches through the server's relay end.",
                "Restart", lambda: self.service("restart", self.server.unit, self.server_row)))
        self.https_row = Adw.ActionRow(title="Public name", subtitle="…")
        self.https_row.set_subtitle_selectable(True)
        self.https_icon = Gtk.Image(icon_name="content-loading-symbolic")
        self.https_row.add_prefix(self.https_icon)
        self.server_boot_row = Adw.SwitchRow(
            title="Start with this PC",
            subtitle="This server starts when the PC does, before anyone signs in")
        self.server_boot_handler = self.server_boot_row.connect(
            "notify::active", lambda *_: self.set_boot(
                self.server_boot_row, [self.server.unit], "This server"))
        for row in (self.server_row, self.https_row, self.server_boot_row):
            self.this_server.add(row)

        players = Adw.PreferencesGroup(
            title="What players' games use",
            description="From this server's profile in the game folder's "
                        "data/online/servers.json, which every copy you share carries. "
                        "Change it on the Public name page.")
        self.address_row = info_row("Address")
        self.key_row = info_row("Server key", copy_text="", window=self)
        self.pools_row = info_row("Paired by")
        for row in (self.address_row, self.key_row, self.pools_row):
            players.add(row)

        console = Adw.PreferencesGroup(
            title="Nakama console",
            description="The server's admin pages: accounts, storage, matches. Only this PC "
                        "can open them.")
        self.console_row = Adw.ActionRow(title="Console", subtitle="…")
        self.console_row.add_suffix(icon_button("dialog-password-symbolic",
                                                "Copy the console password",
                                                self.copy_console_password, "Password"))
        self.console_row.add_suffix(icon_button("web-browser-symbolic", "Open in the browser",
                                                self.open_console, "Open"))
        console.add(self.console_row)

        shared = Adw.PreferencesGroup(
            title="Shared by all the servers",
            description="One Caddy answers for every server's public name, and one updater "
                        "keeps the DuckDNS names pointing at this PC.")
        self.caddy_row = ServiceRow(
            "HTTPS (Caddy)",
            lambda: self.service("start", caddy_unit(), self.caddy_row),
            lambda: self.confirm(
                "Stop Caddy?",
                "Players' games cannot reach any of the servers through their public names "
                "until you start it again. Play on this PC still works.",
                "Stop", lambda: self.service("stop", caddy_unit(), self.caddy_row), True),
            lambda: self.service("restart", caddy_unit(), self.caddy_row))
        self.duckdns_row = Adw.ActionRow(title="DuckDNS names", subtitle="…")
        self.duckdns_icon = Gtk.Image(icon_name="content-loading-symbolic")
        self.duckdns_row.add_prefix(self.duckdns_icon)
        self.duckdns_now = icon_button("view-refresh-symbolic",
                                       "Point the names at this network's address now",
                                       self.update_duckdns)
        self.duckdns_row.add_suffix(self.duckdns_now)
        self.shared_boot_row = Adw.SwitchRow(
            title="Start with this PC",
            subtitle="Caddy and the DuckDNS updater start when the PC does")
        self.shared_boot_handler = self.shared_boot_row.connect(
            "notify::active", lambda *_: self.set_boot(
                self.shared_boot_row, [caddy_unit(), duckdns_timer()], "Caddy and DuckDNS"))
        for row in (self.caddy_row, self.duckdns_row, self.shared_boot_row):
            shared.add(row)

        self.all_servers = Adw.PreferencesGroup(
            title="All the servers",
            description="Each has its own service, port and players. Choose one to show it "
                        "on these pages; add or remove them in the menu (☰).")

        for group in (self.this_server, players, console, shared, self.all_servers):
            page.add(group)
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(self.banner)
        box.append(page)
        page.set_vexpand(True)
        return box

    def refuse_foreign(self, units):
        """Whether one of the units is another folder's (then it says so)."""
        foreign = [u for u in units if unit_elsewhere(u)]
        if foreign:
            self.report("Not this folder's service",
                        f"{', '.join(foreign)} belongs to another folder, or settings.env's "
                        "KIT_ID is not an id (lowercase letters, digits and dashes). "
                        "Give this folder its own KIT_ID.")
        return bool(foreign)

    def service(self, verb, unit, row):
        if self.refuse_foreign([unit]):
            return

        def done(code, out):
            if code != 0:
                self.report(f"Could not {verb} {unit}", out)
            else:
                self.toast({"start": "Started", "stop": "Stopped", "restart": "Restarted"}[verb]
                           + " " + unit.removesuffix(".service"))
        self.command(SYSTEMCTL + [verb, unit], done, busy=row.set_busy, timeout=90)

    def set_boot(self, row, units, what):
        on = row.get_active()
        if self.refuse_foreign(units):
            return

        def done(code, out):
            if code != 0:
                self.report("Could not change what starts with this PC", out)
            else:
                self.toast(f"{what}: {'starts' if on else 'no longer starts'} with this PC")
        self.command(SYSTEMCTL + ["enable" if on else "disable", *units], done,
                     busy=lambda b: row.set_sensitive(not b))

    def update_duckdns(self):
        if not os.path.exists(DUCKDNS_ENV):
            self.toast("Save a DuckDNS token on the Public name page first")
            return
        if self.refuse_foreign([duckdns_unit()]):
            return

        def done(code, out):
            if code != 0:
                self.report("The DuckDNS update failed", out + "\n\n" + self.duckdns_log())
            else:
                self.toast(self.duckdns_last_line() or "DuckDNS updated")
        self.command(SYSTEMCTL + ["start", duckdns_unit()], done,
                     busy=lambda b: self.duckdns_now.set_sensitive(not b), timeout=60)

    def duckdns_log(self, lines=8):
        return mask(run(JOURNALCTL + ["-u", duckdns_unit(), "-n", str(lines), "--no-pager", "-q",
                                      "-o", "cat"], timeout=10)[1])

    def duckdns_last_line(self):
        lines = [line for line in self.duckdns_log(20).splitlines()
                 if line and not line.startswith(("Starting ", "Finished ", "Started ",
                                                  kit_id() + "-"))]
        return lines[-1] if lines else ""

    def load_console(self):
        """The console's port and user: from config/servers.json, else from
        the server's settings (show, without --secrets)."""
        s = self.server
        if s.console_port:
            self.console_row.set_subtitle(f"http://127.0.0.1:{s.console_port} · user "
                                          f"{s.console_user or '?'}")
            return

        def work():
            code, out = run([SERVER_SH, "show", s.id], timeout=30)
            listen = re.search(r"Listens on:.*?port (\d+)", out)
            m = re.search(r"Console:\s+port (\d+).*?user (\S+?),", out)
            return ((int(listen.group(1)) if code == 0 and listen else None),
                    (int(m.group(1)), m.group(2)) if code == 0 and m else (None, None))

        def done(result):
            port, (console, user) = result
            s.port = s.port or port
            s.console_port, s.console_user = console, user
            if s is not self.server:
                return
            self.refresh()
            if console:
                self.console_row.set_subtitle(f"http://127.0.0.1:{console} · user {user}")
            else:
                self.console_row.set_subtitle(f"Not found: ./server.sh show {s.id} says where")
        in_background(work, done)

    def open_console(self):
        if not self.server.console_port:
            self.toast("The console's port is not known")
            return
        Gtk.UriLauncher.new(f"http://127.0.0.1:{self.server.console_port}").launch(
            self, None, None)

    def copy_console_password(self):
        sid = self.server.id

        def work():
            code, out = run([SERVER_SH, "show", sid, "--secrets"], timeout=30)
            m = re.search(r"^\s*Console:.*\bpassword (\S+)\s*$", out, re.M)
            return m.group(1) if code == 0 and m else None

        def done(password):
            if password:
                self.copy(password, f"Console password of {sid} copied")
            else:
                self.toast(f"The password was not found: ./server.sh show {sid} --secrets")
        in_background(work, done)

    # Public name page ----------------------------------------------------------

    def public_page(self):
        page = Adw.PreferencesPage()
        self.public_banner = Adw.Banner(revealed=False)

        self.name_group = Adw.PreferencesGroup(
            title="Public name",
            description=f"The name players' games connect to, on port 443 with TLS. Your "
                        f"router forwards port 443 to this PC's port {caddy_https_port()}, where "
                        "Caddy answers with a Let's Encrypt certificate for this name. Each "
                        "server has a name of its own.")
        self.host_entry = Adw.EntryRow(title="Public name, such as mygame.duckdns.org")
        self.name_group.add(self.host_entry)

        pools = Adw.PreferencesGroup(
            title="Who plays together",
            description="Players are only paired with the same game id and build. Change the "
                        "build whenever the game you share changes: characters, stages, "
                        "screenpack, save/config.ini's [Common] section, or the engine (a new "
                        "Ikemen_GO program). Otherwise old and new copies are paired, and their "
                        "matches fail or desync.")
        self.build_entry = Adw.EntryRow(title="Build")
        self.game_entry = Adw.EntryRow(title="Game id")
        self.region_entry = Adw.EntryRow(title="Region")
        for row in (self.build_entry, self.game_entry, self.region_entry):
            pools.add(row)

        folder = Adw.PreferencesGroup(
            title="Game folder",
            description="The folder you share with this server's players. Applying writes "
                        "this server's profile into its data/online/servers.json, next to "
                        "the profiles of other servers there.")
        self.folder_row = Adw.ActionRow(title="Folder", subtitle="…")
        self.folder_row.set_subtitle_selectable(True)
        self.folder_row.add_suffix(icon_button("folder-open-symbolic", "Choose the game folder",
                                               self.choose_folder, "Choose…"))
        folder.add(self.folder_row)

        apply = Adw.PreferencesGroup()
        self.apply_button = Gtk.Button(label="Apply", halign=Gtk.Align.CENTER)
        self.apply_button.add_css_class("pill")
        self.apply_button.add_css_class("suggested-action")
        self.apply_button.connect("clicked", lambda _b: self.ask_apply())
        apply.add(self.apply_button)

        self.duckdns_group = Adw.PreferencesGroup(
            title="DuckDNS, for all the servers",
            description="…")
        self.duckdns_entry = Adw.EntryRow(
            title="Other DuckDNS names, comma-separated (without .duckdns.org)")
        self.token_entry = Adw.PasswordEntryRow(title="Token")
        save = Gtk.Button(label="Save", halign=Gtk.Align.CENTER)
        save.add_css_class("pill")
        save.connect("clicked", lambda _b: self.save_duckdns())
        self.duckdns_group.add(self.token_entry)
        self.duckdns_group.add(self.duckdns_entry)
        self.token_hint = Gtk.Label(xalign=0, wrap=True, margin_top=6)
        self.token_hint.add_css_class("dim-label")
        self.token_hint.add_css_class("caption")
        self.duckdns_group.add(self.token_hint)
        duckdns_save = Adw.PreferencesGroup()
        duckdns_save.add(save)

        for group in (self.name_group, pools, folder, apply, self.duckdns_group, duckdns_save):
            page.add(group)
        for entry in (self.host_entry, self.build_entry, self.game_entry, self.region_entry):
            entry.connect("changed", lambda *_: self.check_unapplied())
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(self.public_banner)
        box.append(page)
        page.set_vexpand(True)
        return box

    def auto_duckdns_names(self):
        """The DuckDNS names the updater keeps up to date by itself: the
        servers' public names that end in .duckdns.org."""
        names = []
        for sid in server_ids():
            label = duckdns_label(read_env(os.path.join(SERVERS_DIR, sid + ".env"))
                                  .get("PUBLIC_HOST", "").strip())
            if label and label not in names:
                names.append(label)
        return names

    def load_settings(self):
        """Fills the Public name and Play pages from the files."""
        s = self.server
        s.load()
        settings = s.settings()
        self.game_dir = settings["GAME_DIR"]
        self.host_entry.set_text(s.public_host)
        self.build_entry.set_text(settings["BUILD"])
        self.game_entry.set_text(settings["GAME"])
        self.region_entry.set_text(settings["REGION"])
        self.show_folder()
        self.load_duckdns()
        self.show_profiles()
        self.check_unapplied()

    def load_duckdns(self):
        duckdns = read_env(DUCKDNS_ENV)
        names = [n.strip().removesuffix(".duckdns.org")
                 for n in unquote(duckdns.get("DUCKDNS_DOMAIN", "")).split(",") if n.strip()]
        self.duckdns_entry.set_text(",".join(names))
        self.token_entry.set_text("")
        auto = self.auto_duckdns_names()
        self.duckdns_group.set_description(
            "Keeps the names pointing at your home's address when your internet provider "
            "changes it: every 5 minutes, ddns/duckdns-update.sh tells DuckDNS, for all the "
            "names in one go. The servers' public names that end in .duckdns.org are "
            "updated by themselves"
            + (f" (now: {', '.join(n + '.duckdns.org' for n in auto)})" if auto else "")
            + "; add any other names below. The token is on duckdns.org once you sign in; "
            "it is saved only on this PC (ddns/duckdns.env, readable by you alone) and never "
            "shown here.")
        self.token_hint.set_label(
            "A token is saved. Leave Token empty to keep it, or type a new one to replace it."
            if unquote(duckdns.get("DUCKDNS_TOKEN", "")) else "No token saved yet.")

    def show_folder(self):
        if not self.game_dir:
            self.folder_row.set_subtitle("Not chosen yet")
            return
        ok = os.path.isdir(os.path.join(self.game_dir, "data", "online"))
        self.folder_row.set_subtitle(esc(self.game_dir + ("" if ok else "  (no data/online folder here)")))

    def choose_folder(self):
        dialog = Gtk.FileDialog(title="The game folder you share")
        if os.path.isdir(self.game_dir):
            dialog.set_initial_folder(Gio.File.new_for_path(self.game_dir))
        server = self.server

        def chosen(_dialog, result):
            try:
                folder = dialog.select_folder_finish(result)
            except GLib.Error:
                return  # cancelled
            path = folder.get_path() if folder else None
            if not path or server is not self.server:
                return
            if "\n" in path or "\r" in path:
                self.toast("That folder's name cannot be used")
                return
            self.game_dir = path
            self.show_folder()
            self.check_unapplied()
        dialog.select_folder(self, None, chosen)

    def pending(self):
        """What Apply would change, in words; empty when nothing would."""
        shared = find_profile(shared_profile_path(self.game_dir), self.server.id)
        if not shared:
            # No profile yet: only what differs from servers/<id>.env counts.
            saved = self.server.settings()
            shared = {"host": "", "build": saved["BUILD"], "game": saved["GAME"],
                      "region": saved["REGION"]}
        changes = []
        if self.host_entry.get_text().strip() != shared.get("host", ""):
            changes.append("public name")
        for key, entry in (("build", self.build_entry), ("game", self.game_entry),
                           ("region", self.region_entry)):
            if entry.get_text().strip() != shared.get(key, ""):
                changes.append({"game": "game id"}.get(key, key))
        if self.game_dir != self.server.game_dir:
            changes.append("game folder")
        return changes

    def check_unapplied(self):
        if not self.server:
            return
        changes = self.pending()
        if changes:
            self.public_banner.set_title(esc(f"Not applied yet for {self.server.title}: "
                                             + ", ".join(changes)))
        self.public_banner.set_revealed(bool(changes))

    def ask_apply(self):
        s = self.server
        host = self.host_entry.get_text().strip()
        values = {"BUILD": self.build_entry.get_text().strip(),
                  "GAME": self.game_entry.get_text().strip(),
                  "REGION": self.region_entry.get_text().strip()}
        if not HOST_RE.match(host) or re.fullmatch(r"[0-9.]+", host):
            self.toast("The public name is a full host name: lowercase letters, digits, dots, dashes")
            return
        for key, value in values.items():
            if not WORD_RE.match(value):
                self.toast(f"{key.title()}: letters, digits, dots, dashes and underscores only")
                return
        for other in server_ids():
            if other != s.id and read_env(os.path.join(SERVERS_DIR, other + ".env")).get(
                    "PUBLIC_HOST", "").strip() == host:
                self.toast(f"The server {other} has this public name already: each server "
                           "needs a name of its own")
                return
        if not os.path.isdir(os.path.join(self.game_dir, "data", "online")):
            self.toast("Choose the game folder: it has no data/online folder")
            return
        shared = find_profile(shared_profile_path(self.game_dir), s.id) or {}
        build_note = ""
        if shared.get("build") and shared["build"] != values["BUILD"]:
            build_note = (f"\n\nThe build changes from {shared['build']} to {values['BUILD']}: "
                          "copies with the old build are no longer paired with the new ones.")
        self.confirm(
            f"Apply these settings to {s.title}?",
            f"This writes {host}, build {values['BUILD']}, game id {values['GAME']} and region "
            f"{values['REGION']} into {s.id}'s profile in the game folder's "
            f"data/online/servers.json and into host-profiles/{s.id}.json, gives the server "
            "its new public address, and restarts Caddy (shared by all the servers), which "
            "then gets a certificate for the name if it has none."
            f"{build_note}\n\nPlayers only get the change in a new copy of the game folder: "
            "rebuild the zips (package-game-zip.py) and share them again.",
            "Apply", lambda: self.apply(s, host, values))

    def apply(self, server, host, values):
        # servers/<id>.env first: set-public-host.sh reads it.
        try:
            update_env(server.env_path, dict(values, GAME_DIR=self.game_dir))
        except OSError as e:
            self.report(f"Could not save servers/{server.id}.env", str(e))
            return

        def busy(b):
            self.apply_button.set_sensitive(not b)
            self.apply_button.set_label("Applying…" if b else "Apply")

        def done(code, out):
            if code != 0:
                self.report("set-public-host.sh failed", out)
            else:
                self.toast(f"Applied to {server.title}. Rebuild the zips before you share "
                           "the game folder.")
            if server.id == self.server.id:
                self.load_settings()
        self.command([SET_PUBLIC_HOST_SH, server.id, host], done, busy=busy, timeout=180)

    def save_duckdns(self):
        names = []
        for name in self.duckdns_entry.get_text().split(","):
            name = name.strip().removesuffix(".duckdns.org")
            if not name:
                continue
            if not DUCKDNS_RE.match(name):
                self.toast(f"The DuckDNS name {name}: letters, digits, dots and dashes")
                return
            if name not in names:
                names.append(name)
        token = self.token_entry.get_text().strip()
        saved = read_env(DUCKDNS_ENV)
        if not token:
            token = unquote(saved.get("DUCKDNS_TOKEN", ""))
        if not token or not DUCKDNS_RE.match(token):
            self.toast("The token: letters, digits, dots and dashes, as duckdns.org shows it")
            return
        if not names and not self.auto_duckdns_names():
            self.toast("No DuckDNS name yet: give a server a .duckdns.org public name, or "
                       "add a name here")
            return
        text = ("# The DuckDNS token (ddns/duckdns-update.sh), and the names it updates\n"
                "# besides the servers' .duckdns.org public names. Keep this file\n"
                "# readable by you alone: the token lets anyone move the names.\n"
                f"DUCKDNS_DOMAIN={','.join(names)}\nDUCKDNS_TOKEN={token}\n")
        try:
            os.makedirs(os.path.dirname(DUCKDNS_ENV), exist_ok=True)
            write_file(DUCKDNS_ENV, text, 0o600)
        except OSError as e:
            self.report("Could not save ddns/duckdns.env", str(e))
            return
        self.load_duckdns()
        self.toast("Saved. Updating DuckDNS…")
        self.update_duckdns()

    # Play page -----------------------------------------------------------------

    def play_page(self):
        page = Adw.PreferencesPage()
        intro = Adw.PreferencesGroup(
            title="Play on this PC",
            description="Starts the game from the server's game folder signed in to the "
                        "server on this PC directly (127.0.0.1, no TLS), with its "
                        "host-profiles/&lt;id&gt;.json. Your router cannot reach its own public "
                        "address from inside your network, so the public name does not work "
                        "here. Your account, matches and lobbies are the same as everyone "
                        "else's.")
        self.profile_row = Adw.ActionRow(title="This PC's profile", subtitle="…")
        self.profile_row.set_subtitle_selectable(True)
        self.profile_icon = Gtk.Image(icon_name="object-select-symbolic")
        self.profile_row.add_prefix(self.profile_icon)
        self.play_folder_row = Adw.ActionRow(title="Game folder", subtitle="…")
        self.play_folder_row.set_subtitle_selectable(True)
        intro.add(self.profile_row)
        intro.add(self.play_folder_row)
        play = Adw.PreferencesGroup()
        self.play_button = Gtk.Button(halign=Gtk.Align.CENTER)
        self.play_button.set_child(Adw.ButtonContent(icon_name="media-playback-start-symbolic",
                                                     label="Play"))
        self.play_button.add_css_class("pill")
        self.play_button.add_css_class("suggested-action")
        self.play_button.connect("clicked", lambda _b: self.ask_play())
        play.add(self.play_button)
        page.add(intro)
        page.add(play)
        return page

    def show_profiles(self):
        """The two profiles: the shared one (Server page) and this PC's."""
        s = self.server
        shared = s.shared_profile()
        if shared:
            port = shared.get("port", "?")
            tls = "TLS" if shared.get("ssl") else "no TLS"
            self.address_row.set_subtitle(f"{shared.get('host', '?')}, port {port}, {tls}")
            self.key_row.set_subtitle(shared.get("server_key", ""))
            self.key_row.copy_value = shared.get("server_key", "")
            self.pools_row.set_subtitle(f"game id {shared.get('game', '?')}, build "
                                        f"{shared.get('build', '?')}, region "
                                        f"{shared.get('region', '?')}")
        else:
            self.address_row.set_subtitle(f"No profile for {s.id} in the game folder's "
                                          "data/online/servers.json yet")
            self.key_row.set_subtitle("")
            self.key_row.copy_value = ""
            self.pools_row.set_subtitle("")
        self.play_folder_row.set_subtitle(esc(s.game_dir) if s.game_dir else "Not chosen yet")
        mine = find_profile(s.host_profile)
        problem = self.play_problem(mine, shared)
        if mine:
            self.profile_row.set_subtitle(
                f"{mine.get('host')}:{mine.get('port')}, {'TLS' if mine.get('ssl') else 'no TLS'}"
                f", game id {mine.get('game')}, build {mine.get('build')}"
                + (f"\n{problem}" if problem else ""))
        else:
            self.profile_row.set_subtitle(problem)
        self.profile_icon.set_from_icon_name("dialog-warning-symbolic" if problem
                                             else "object-select-symbolic")
        for css in ("success", "warning"):
            self.profile_icon.remove_css_class(css)
        self.profile_icon.add_css_class("warning" if problem else "success")

    def play_problem(self, mine, shared):
        s = self.server
        if not s.game_dir:
            return "Choose the game folder on the Public name page, and apply."
        if not mine:
            return (f"There is no host-profiles/{s.id}.json: apply the public name once "
                    "to make it.")
        if not shared:
            return f"The game folder's data/online/servers.json has no profile for {s.id}."
        if mine.get("build") != shared.get("build") or mine.get("game") != shared.get("game"):
            return (f"It is for build {mine.get('build')}, the game folder for "
                    f"{shared.get('build')}: apply the public name again.")
        if not os.path.exists(os.path.join(s.game_dir, "Ikemen_GO_Linux")):
            return "The game folder has no Ikemen_GO_Linux."
        return ""

    def ask_play(self):
        s = self.server
        s.load()
        problem = self.play_problem(find_profile(s.host_profile), s.shared_profile())
        if problem:
            self.toast(problem)
            return
        if not self.server_running:
            self.confirm(f"{s.title} is not running",
                         "The game starts, but cannot sign in until you start the server.",
                         "Play anyway", self.play)
            return
        self.play()

    def play(self):
        s = self.server
        log_dir = os.path.join(GLib.get_user_cache_dir(), "server-control", kit_id(), s.id)
        os.makedirs(log_dir, exist_ok=True)
        log = os.path.join(log_dir, "play-on-this-pc.log")
        try:
            with open(log, "w", encoding="utf-8") as out:
                game = subprocess.Popen([PLAY_SH, s.id], stdin=subprocess.DEVNULL,
                                        stdout=out, stderr=subprocess.STDOUT, cwd=HERE,
                                        start_new_session=True)
        except OSError as e:
            self.report("Could not start the game", str(e))
            return
        self.play_button.set_sensitive(False)
        result = {}

        def wait():
            result["code"] = game.wait()  # also reaps it when the game ends
        threading.Thread(target=wait, daemon=True).start()

        def check():
            self.play_button.set_sensitive(True)
            code = result.get("code")
            if code not in (None, 0):
                self.report("The game did not start", tail(log, 40))
            elif code is None:
                self.toast("The game is starting")
            return False
        GLib.timeout_add(2000, check)

    # Logs page -----------------------------------------------------------------

    def journal(self, unit):
        return run(JOURNALCTL + ["-u", unit, "-n", str(LOG_LINES), "--no-pager", "-q",
                                 "-o", "short"], timeout=15)[1]

    def logs_page(self):
        # Each reads the server shown when it runs.
        self.log_sources = [
            lambda s: self.journal(s.unit),
            lambda s: tail(s.nakama_log, LOG_LINES),
            lambda s: self.journal(caddy_unit()),
            lambda s: self.journal(duckdns_unit()),
        ]
        self.log_names = Gtk.StringList.new(["Game server", "Nakama's own log",
                                             "HTTPS (Caddy, all the servers)",
                                             "DuckDNS (all the names)"])
        bar = Gtk.Box(spacing=12, margin_top=12, margin_bottom=6, margin_start=12, margin_end=12)
        self.log_choice = Gtk.DropDown(model=self.log_names)
        self.log_choice.connect("notify::selected", lambda *_: self.load_log())
        self.follow = Gtk.Switch(valign=Gtk.Align.CENTER, active=True,
                                 tooltip_text="Refresh every few seconds")
        follow_label = Gtk.Label(label="Follow")
        refresh = icon_button("view-refresh-symbolic", "Refresh", self.load_log)
        spacer = Gtk.Box(hexpand=True)
        for widget in (self.log_choice, spacer, follow_label, self.follow, refresh):
            bar.append(widget)
        self.log_view = Gtk.TextView(editable=False, monospace=True, cursor_visible=False,
                                     wrap_mode=Gtk.WrapMode.WORD_CHAR, top_margin=8,
                                     bottom_margin=8, left_margin=10, right_margin=10)
        self.log_scroll = Gtk.ScrolledWindow(child=self.log_view, vexpand=True,
                                             margin_start=12, margin_end=12, margin_bottom=12)
        self.log_scroll.add_css_class("card")
        self.log_loading = False
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(bar)
        box.append(self.log_scroll)
        return box

    def load_log(self):
        if self.log_loading or not self.server:
            return
        self.log_loading = True
        index = self.log_choice.get_selected()
        source = self.log_sources[index]
        server = self.server

        def done(text):
            self.log_loading = False
            if index != self.log_choice.get_selected() or server is not self.server:
                # Another log was asked for meanwhile: load_log() gave up then.
                if self.stack.get_visible_child_name() == "logs":
                    self.load_log()
                return
            adj = self.log_scroll.get_vadjustment()
            at_end = adj.get_value() >= adj.get_upper() - adj.get_page_size() - 40
            buffer = self.log_view.get_buffer()
            text = mask(text) or "(empty)"
            if buffer.get_text(buffer.get_start_iter(), buffer.get_end_iter(), False) == text:
                return
            buffer.set_text(text)
            if at_end:
                GLib.idle_add(lambda: adj.set_value(adj.get_upper()) and False)
        in_background(lambda: source(server), done)

    def page_changed(self):
        # The Public name page keeps what was typed until Apply.
        if self.stack.get_visible_child_name() == "logs":
            self.load_log()
        elif self.stack.get_visible_child_name() == "play":
            self.show_profiles()

    # Status ------------------------------------------------------------------

    def tick(self):
        if server_ids() != self.ids:
            self.reload_servers()  # added or removed by the scripts
            if not self.ids or not self.get_application():
                return False
        self.refresh()
        if self.stack.get_visible_child_name() == "logs" and self.follow.get_active():
            self.load_log()
        return True

    def refresh(self):
        """Reads the services' states and health in the background."""
        if self.refreshing:
            self.refresh_again = True  # after this one: something just changed
            return
        if not self.server:
            return
        self.refreshing = True
        self.refresh_again = False
        servers = [Server(sid) for sid in self.ids]
        selected = self.server
        for s in servers:
            if s.id == selected.id and not s.port:
                s.port = selected.port  # from "show <id>"
        kit_units = [caddy_unit(), duckdns_unit(), duckdns_timer()]
        https_port = caddy_https_port()

        def work():
            units, error = unit_states([s.unit for s in servers] + kit_units)
            health = {}
            for s in servers:
                if s.port and units.get(s.unit, {}).get("ActiveState") == "active":
                    health[s.id] = check_nakama(s.port)
            if units.get(kit_units[0], {}).get("ActiveState") == "active":
                for s in servers:
                    if s.public_host and s.has_site():
                        health["https:" + s.public_host] = check_https(s.public_host, https_port)
            return servers, units, error, health

        def done(result):
            self.refreshing = False
            if self.server:
                self.show_status(*result)
            if self.refresh_again:
                self.refresh()
        in_background(work, done)

    def server_state(self, s, units, health):
        """(state, words, problem or None) for one server."""
        unit = units.get(s.unit, {})
        state, text = describe(unit)
        problem = None
        if unit.get("ActiveState") == "active":
            ok, msg = health.get(s.id, (False, "checking"))
            if ok:
                text += " · answers on this PC"
            elif msg == "checking":
                text += " · checking…"
            else:
                state, text = "warning", text + f" · not answering yet ({msg})"
                problem = "is not answering"
        elif state == "error":
            problem = "failed"
        elif not running(unit):
            problem = "is stopped"
        return state, text, problem

    def show_status(self, servers, units, error, health):
        self.units = units
        s = next((x for x in servers if x.id == self.server.id), self.server)
        caddy = units.get(caddy_unit(), {})
        duck = units.get(duckdns_unit(), {})
        timer = units.get(duckdns_timer(), {})
        problems = []
        caddy_running = running(caddy)

        # Every server in the list.
        for other in servers:
            entry = self.server_rows.get(other.id)
            state, text, problem = self.server_state(other, units, health)
            if entry:
                row, icon, _check = entry
                show_state(icon, state)
                row.set_subtitle(" · ".join(
                    part for part in (other.id, f"port {other.port}" if other.port else "",
                                      other.public_host, text) if part))
            if problem and other.id != s.id and state == "error":
                problems.append(f"{other.id} failed")

        # The server shown.
        server_unit = units.get(s.unit, {})
        self.server_running = running(server_unit)
        state, text, problem = self.server_state(s, units, health)
        if problem:
            problems.insert(0, f"{s.id} {problem}")
        self.server_row.show(state, text, self.server_running)

        if not s.public_host or not s.has_site():
            self.https_row.set_subtitle("None yet: set it on the Public name page")
            show_state(self.https_icon, "warning")
            problems.append(f"{s.id} has no public name")
        elif not caddy_running:
            self.https_row.set_subtitle(f"https://{s.public_host} · Caddy is not running")
            show_state(self.https_icon, "warning")
        else:
            ok, msg = health.get("https:" + s.public_host, (False, "checking"))
            if ok:
                self.https_row.set_subtitle(f"https://{s.public_host} answers")
                show_state(self.https_icon, "ok")
            else:
                self.https_row.set_subtitle(f"https://{s.public_host} does not answer yet ({msg})")
                show_state(self.https_icon, "warning")
                if msg != "checking":
                    problems.append(f"{s.public_host} is not answering")

        # Caddy, for all the names.
        state, text = describe(caddy)
        named = [x for x in servers if x.public_host and x.has_site()]
        if not site_files():
            state, text = "warning", "Waiting for a public name (Public name page)"
        elif caddy.get("ActiveState") == "active":
            failing = [x.public_host for x in named
                       if not health.get("https:" + x.public_host, (False, ""))[0]]
            if not named:
                pass
            elif not failing:
                text += (f" · https://{named[0].public_host} answers" if len(named) == 1
                         else f" · all {len(named)} names answer")
            else:
                state = "warning"
                text += " · not answering yet: " + ", ".join(failing)
        elif not caddy_running:
            problems.append("Caddy is stopped")
        self.caddy_row.show(state, text, caddy_running)

        if not os.path.exists(DUCKDNS_ENV):
            self.show_duckdns("warning", "No DuckDNS token yet (Public name page)")
        elif timer.get("ActiveState") != "active":
            self.show_duckdns("warning", "The updater is off: it runs only when you press ↻")
        else:
            last = when(duck.get("ExecMainExitTimestamp", "")) or when(timer.get("LastTriggerUSec", ""))
            if duck.get("Result", "success") != "success" or duck.get("ExecMainStatus", "0") not in ("0", ""):
                self.show_duckdns("error", f"The last update failed{' at ' + last if last else ''}: see Logs")
                problems.append("the DuckDNS update failed")
            else:
                self.show_duckdns("ok", "Updated every 5 minutes"
                                  + (f", last at {last}" if last else ""))

        for row, handler, names in (
                (self.server_boot_row, self.server_boot_handler, [s.unit]),
                (self.shared_boot_row, self.shared_boot_handler, [caddy_unit(), duckdns_timer()])):
            row.handler_block(handler)
            row.set_active(all(units.get(u, {}).get("UnitFileState") == "enabled" for u in names))
            row.handler_unblock(handler)

        if server_unit.get("LoadState") == "not-found":
            problems = [f"the services of {s.id} are not installed (./install-services.sh, "
                        "README.md)"]
        if error:
            self.banner.set_title(esc("systemctl did not answer: " + error.splitlines()[-1][:120]))
        elif problems:
            self.banner.set_title(esc("Players cannot play right now: " + "; ".join(problems)))
        self.banner.set_revealed(bool(error or problems))
        self.show_profiles()

    def show_duckdns(self, state, text):
        self.duckdns_row.set_subtitle(text)
        show_state(self.duckdns_icon, state)


class SetupWindow(Dialogs, Adw.ApplicationWindow):
    """Shown while the kit has no server: makes the first one."""

    def __init__(self, app, problem=None):
        super().__init__(application=app, title=kit_title(), default_width=560,
                         default_height=480)
        view = Adw.ToolbarView()
        view.add_top_bar(Adw.HeaderBar())
        self.toasts = Adw.ToastOverlay()
        add = Gtk.Button(label="Add a server…", halign=Gtk.Align.CENTER)
        add.add_css_class("pill")
        add.add_css_class("suggested-action")
        add.connect("clicked", lambda _b: self.ask_add_server(self.added))
        description = (
            "This folder has no server yet. Add one: it gets an id, its own port and "
            "database, and a game folder. You can add more later, for other games or "
            "builds. Put the programs in place first (README.md, next to this program, "
            f"says how): {HERE}")
        if problem:
            description = ("The kit could not be brought up to date (upgrade-kit.sh):\n"
                           + mask(problem)[-600:] + "\n\n" + description)
        status = Adw.StatusPage(icon_name="network-server-symbolic",
                                title="Add the first server", description=description,
                                child=add)
        self.toasts.set_child(status)
        view.set_content(self.toasts)
        self.set_content(view)

    def added(self, sid):
        self.get_application().show_main(sid)
        self.close()


class App(Adw.Application):
    def __init__(self, app_id, upgrade_problem=None):
        super().__init__(application_id=app_id, flags=Gio.ApplicationFlags.DEFAULT_FLAGS)
        self.upgrade_problem = upgrade_problem

    def do_activate(self):
        window = self.get_active_window() or (
            Window(self) if server_ids() else SetupWindow(self, self.upgrade_problem))
        window.present()

    def show_main(self, select=None):
        Window(self, select).present()

    def show_setup(self):
        SetupWindow(self, self.upgrade_problem).present()


def upgrade():
    """Brings a kit from before servers/ (settings.env with SERVER_ID) to
    servers/<id>.env; upgrade-kit.sh keeps its service names. The problem in
    words, or None."""
    if not legacy_layout():
        return None
    code, out = run([UPGRADE_KIT_SH], timeout=120)
    return None if code == 0 else (out or f"upgrade-kit.sh ended with {code}")


if __name__ == "__main__":
    problem = upgrade()
    raise SystemExit(App(app_id_for(kit_id()), problem).run(None))
