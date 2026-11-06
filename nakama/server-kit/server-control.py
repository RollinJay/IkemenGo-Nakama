#!/usr/bin/env python3
"""A window for running an IKEMEN GO game's online server with this kit.

It does with buttons what the files next to it do:
- the <id>-server, <id>-caddy and <id>-duckdns services (systemctl --user;
  install-services.sh installs them): start, stop, restart, and whether they
  start with this PC;
- set-public-host.sh: the public name, and the game id, build and region
  players are paired by (kept in settings.env, which the scripts read);
- ddns/duckdns.env: the DuckDNS name and token;
- play-on-this-pc.sh with host-profile.json: the game on this PC, signed in to
  the server directly;
- the logs.

    python3 server-control.py

It needs Python 3 with PyGObject, GTK 4 and libadwaita 1.5 or newer (Debian
and Ubuntu: python3-gi gir1.2-gtk-4.0 gir1.2-adw-1), and Linux with systemd.
Tests point it at another kit folder with SERVER_KIT_DIR, replace systemctl
and journalctl with SERVER_KIT_SYSTEMCTL and SERVER_KIT_JOURNALCTL, and open a
page with SERVER_KIT_PAGE (server, public, play or logs).
"""
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
PUBLIC_HOST_ENV = os.path.join(HERE, "caddy", "public-host.env")
CADDYFILE = os.path.join(HERE, "caddy", "Caddyfile")
HOST_PROFILE = os.path.join(HERE, "host-profile.json")
DUCKDNS_ENV = os.path.join(HERE, "ddns", "duckdns.env")
SERVER_SH = os.path.join(HERE, "server.sh")
SET_PUBLIC_HOST_SH = os.path.join(HERE, "set-public-host.sh")
PLAY_SH = os.path.join(HERE, "play-on-this-pc.sh")

# The names set-public-host.sh accepts: lowercase, at least one dot, no IP.
HOST_RE = re.compile(r"^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$")
WORD_RE = re.compile(r"^[A-Za-z0-9._-]{1,64}$")
DUCKDNS_RE = re.compile(r"^[A-Za-z0-9.-]{1,128}$")
LOG_LINES = 400

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


# The server this folder runs (settings.env), and the names that follow from it.
_SAVED = read_env(SETTINGS)
SERVER_ID = _SAVED.get("SERVER_ID", "").strip()
TITLE = _SAVED.get("TITLE", "").strip() or (f"{SERVER_ID} server" if SERVER_ID else "Game server")
SERVER_UNIT = f"{SERVER_ID}-server.service"
CADDY_UNIT = f"{SERVER_ID}-caddy.service"
DUCKDNS_UNIT = f"{SERVER_ID}-duckdns.service"
DUCKDNS_TIMER = f"{SERVER_ID}-duckdns.timer"
# What "Start with this PC" turns on and off.
BOOT_UNITS = (SERVER_UNIT, CADDY_UNIT, DUCKDNS_TIMER)
NAKAMA_LOG = os.path.join(HERE, "config", "servers", SERVER_ID, "server.log")
# As install-services.sh names the launcher.
_PART = re.sub(r"[^A-Za-z0-9]", "_", SERVER_ID) or "unset"
APP_ID = "local.IkemenServer." + ("s_" + _PART if _PART[0].isdigit() else _PART)
# set-public-host.sh's own defaults (settings.sh), used until settings.env or
# the game folder's profile says otherwise.
DEFAULT_GAME_DIR = ""
DEFAULTS = {"GAME": SERVER_ID, "BUILD": "", "REGION": "us-west"}


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


def default_profile(path):
    """The default server of an IKEMEN GO server profile, or None."""
    try:
        with open(path, encoding="utf-8") as f:
            profile = json.load(f)
        return next(s for s in profile["servers"] if s["id"] == profile["default"])
    except (OSError, ValueError, KeyError, StopIteration, TypeError):
        return None


def shared_profile_path(game_dir):
    return os.path.join(game_dir, "data", "online", "servers.json")


def current_settings():
    """The game folder, game id, build and region in use: settings.env's, else
    those the game folder's profile was made with, else the scripts' own."""
    saved = read_env(SETTINGS)
    game_dir = saved.get("GAME_DIR") or DEFAULT_GAME_DIR
    shared = default_profile(shared_profile_path(game_dir)) or {}
    return {
        "GAME_DIR": game_dir,
        "GAME": saved.get("GAME") or shared.get("game") or DEFAULTS["GAME"],
        "BUILD": saved.get("BUILD") or shared.get("build") or DEFAULTS["BUILD"],
        "REGION": saved.get("REGION") or shared.get("region") or DEFAULTS["REGION"],
    }


def public_host():
    for key, value in read_env(PUBLIC_HOST_ENV).items():
        if key == "PUBLIC_HOST" or key.endswith("_PUBLIC_HOST"):
            return value.strip()
    return ""


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


def unit_states():
    """systemctl show for the four units: {unit: {property: value}}."""
    props = ("Id,LoadState,ActiveState,SubState,Result,ActiveEnterTimestamp,ExecMainExitTimestamp,"
             "ExecMainStatus,UnitFileState,LastTriggerUSec")
    code, out = run(SYSTEMCTL + ["show", "--timestamp=unix", "-p", props, SERVER_UNIT,
                                 CADDY_UNIT, DUCKDNS_UNIT, DUCKDNS_TIMER], timeout=15)
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
    """Asks Caddy on this PC for the server's health under the public name,
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


def in_background(work, done):
    """Runs work() in a thread, then done(its result) in the window's thread."""
    def finish(result):
        done(result)
        return False

    def thread():
        GLib.idle_add(finish, work())
    threading.Thread(target=thread, daemon=True).start()


# Widgets --------------------------------------------------------------------

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

    def show(self, state, text, running):
        """state: ok, warning, error or off."""
        icons = {"ok": "object-select-symbolic", "warning": "dialog-warning-symbolic",
                 "error": "dialog-error-symbolic", "off": "media-playback-stop-symbolic"}
        self.icon.set_from_icon_name(icons[state])
        for css in ("success", "warning", "error", "dim-label"):
            self.icon.remove_css_class(css)
        self.icon.add_css_class({"ok": "success", "warning": "warning",
                                 "error": "error", "off": "dim-label"}[state])
        self.set_subtitle(text)
        self.start.set_visible(not running)
        self.stop.set_visible(running)
        self.restart.set_visible(running)

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


# The window -----------------------------------------------------------------

class Window(Adw.ApplicationWindow):
    def __init__(self, app):
        super().__init__(application=app, title=TITLE, default_width=780,
                         default_height=720)
        self.set_size_request(360, 400)
        self.refreshing = False
        self.refresh_again = False
        self.units = {}
        self.server_running = False
        self.console = {"port": None, "user": None}
        self.nakama_port = None  # from "show <id>"; no health check until known

        self.toasts = Adw.ToastOverlay()
        self.stack = Adw.ViewStack()
        self.toasts.set_child(self.stack)
        header = Adw.HeaderBar()
        switcher = Adw.ViewSwitcher(stack=self.stack, policy=Adw.ViewSwitcherPolicy.WIDE)
        header.set_title_widget(switcher)
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

        self.load_settings()
        self.load_console()
        self.refresh()
        GLib.timeout_add_seconds(4, self.tick)

    # Helpers ---------------------------------------------------------------

    def toast(self, text):
        toast = Adw.Toast.new(text)
        toast.set_timeout(4)
        self.toasts.add_toast(toast)

    def copy(self, text, what):
        Gdk.Display.get_default().get_clipboard().set(text)
        self.toast(what)

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

    # Server page -------------------------------------------------------------

    def server_page(self):
        page = Adw.PreferencesPage()
        self.banner = Adw.Banner(revealed=False)
        services = Adw.PreferencesGroup(
            title="Services",
            description="They run in the background, also with this window closed.")
        self.server_row = ServiceRow(
            "Game server",
            lambda: self.service("start", SERVER_UNIT, self.server_row),
            lambda: self.confirm(
                "Stop the game server?",
                "Players online now are disconnected, and nobody can sign in or find "
                "matches until you start it again.",
                "Stop", lambda: self.service("stop", SERVER_UNIT, self.server_row), True),
            lambda: self.confirm(
                "Restart the game server?",
                "Players online now are disconnected for a few seconds. Matches between "
                "players connected directly go on; matches through the server's relay end.",
                "Restart", lambda: self.service("restart", SERVER_UNIT, self.server_row)))
        self.caddy_row = ServiceRow(
            "HTTPS (Caddy)",
            lambda: self.service("start", CADDY_UNIT, self.caddy_row),
            lambda: self.confirm(
                "Stop Caddy?",
                "Players' games cannot reach the server through its public name until "
                "you start it again. Play on this PC still works.",
                "Stop", lambda: self.service("stop", CADDY_UNIT, self.caddy_row), True),
            lambda: self.service("restart", CADDY_UNIT, self.caddy_row))
        self.duckdns_row = Adw.ActionRow(title="DuckDNS name", subtitle="…")
        self.duckdns_icon = Gtk.Image(icon_name="content-loading-symbolic")
        self.duckdns_row.add_prefix(self.duckdns_icon)
        self.duckdns_now = icon_button("view-refresh-symbolic",
                                       "Point the name at this network's address now",
                                       self.update_duckdns)
        self.duckdns_row.add_suffix(self.duckdns_now)
        self.boot_row = Adw.SwitchRow(
            title="Start with this PC",
            subtitle="The server, Caddy and the DuckDNS updater start when the PC does, "
                     "before anyone signs in")
        self.boot_handler = self.boot_row.connect("notify::active", lambda *_: self.set_boot())
        for row in (self.server_row, self.caddy_row, self.duckdns_row, self.boot_row):
            services.add(row)

        players = Adw.PreferencesGroup(
            title="What players' games use",
            description="From the game folder's data/online/servers.json, which every copy "
                        "you share carries. Change it on the Public name page.")
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

        page.add(services)
        page.add(players)
        page.add(console)
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(self.banner)
        box.append(page)
        page.set_vexpand(True)
        return box

    def service(self, verb, unit, row):
        def done(code, out):
            if code != 0:
                self.report(f"Could not {verb} {unit}", out)
            else:
                self.toast({"start": "Started", "stop": "Stopped", "restart": "Restarted"}[verb]
                           + " " + row.get_title())
        self.command(SYSTEMCTL + [verb, unit], done, busy=row.set_busy, timeout=90)

    def set_boot(self):
        on = self.boot_row.get_active()

        def done(code, out):
            if code != 0:
                self.report("Could not change what starts with this PC", out)
            else:
                self.toast("They start with this PC" if on else "They no longer start with this PC")
        self.command(SYSTEMCTL + ["enable" if on else "disable", *BOOT_UNITS], done,
                     busy=lambda b: self.boot_row.set_sensitive(not b))

    def update_duckdns(self):
        if not os.path.exists(DUCKDNS_ENV):
            self.toast("Save a DuckDNS name and token on the Public name page first")
            return

        def done(code, out):
            if code != 0:
                self.report("The DuckDNS update failed", out + "\n\n" + self.duckdns_log())
            else:
                self.toast(self.duckdns_last_line() or "DuckDNS updated")
        self.command(SYSTEMCTL + ["start", DUCKDNS_UNIT], done,
                     busy=lambda b: self.duckdns_now.set_sensitive(not b), timeout=60)

    def duckdns_log(self, lines=8):
        return mask(run(JOURNALCTL + ["-u", DUCKDNS_UNIT, "-n", str(lines), "--no-pager", "-q",
                                      "-o", "cat"], timeout=10)[1])

    def duckdns_last_line(self):
        lines = [line for line in self.duckdns_log(20).splitlines()
                 if line and not line.startswith(("Starting ", "Finished ", "Started ", SERVER_ID + "-"))]
        return lines[-1] if lines else ""

    def load_console(self):
        """The server's port, and the console's port and user, from the
        server's settings (without --secrets)."""
        def work():
            code, out = run([SERVER_SH, "show", SERVER_ID], timeout=30)
            listen = re.search(r"Listens on:.*?port (\d+)", out)
            m = re.search(r"Console:\s+port (\d+).*?user (\S+?),", out)
            return ((int(listen.group(1)) if code == 0 and listen else None),
                    (int(m.group(1)), m.group(2)) if code == 0 and m else (None, None))

        def done(result):
            self.nakama_port, (port, user) = result
            self.console["port"], self.console["user"] = port, user
            self.refresh()
            if port:
                self.console_row.set_subtitle(f"http://127.0.0.1:{port} · user {user}")
            else:
                self.console_row.set_subtitle(f"Not found: ./server.sh show {SERVER_ID} says where")
        in_background(work, done)

    def open_console(self):
        if not self.console["port"]:
            self.toast("The console's port is not known")
            return
        Gtk.UriLauncher.new(f"http://127.0.0.1:{self.console['port']}").launch(self, None, None)

    def copy_console_password(self):
        def work():
            code, out = run([SERVER_SH, "show", SERVER_ID, "--secrets"], timeout=30)
            m = re.search(r"^\s*Console:.*\bpassword (\S+)\s*$", out, re.M)
            return m.group(1) if code == 0 and m else None

        def done(password):
            if password:
                self.copy(password, "Console password copied")
            else:
                self.toast(f"The password was not found: ./server.sh show {SERVER_ID} --secrets")
        in_background(work, done)

    # Public name page ----------------------------------------------------------

    def public_page(self):
        page = Adw.PreferencesPage()
        self.public_banner = Adw.Banner(revealed=False)

        name = Adw.PreferencesGroup(
            title="Public name",
            description="The name players' games connect to, on port 443 with TLS. Your "
                        "router forwards port 443 to this PC's port 8443, where Caddy answers "
                        "with a Let's Encrypt certificate for this name.")
        self.host_entry = Adw.EntryRow(title="Public name, such as mygame.duckdns.org")
        name.add(self.host_entry)

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
            description="The folder you share with players. Applying writes its "
                        "data/online/servers.json.")
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

        duckdns = Adw.PreferencesGroup(
            title="DuckDNS",
            description="Keeps the name pointing at your home's address when your internet "
                        "provider changes it: every 5 minutes, ddns/duckdns-update.sh tells "
                        "DuckDNS. The token is on duckdns.org once you sign in; it is saved "
                        "only on this PC (ddns/duckdns.env, readable by you alone) and never "
                        "shown here.")
        self.duckdns_entry = Adw.EntryRow(title="DuckDNS name (without .duckdns.org)")
        self.token_entry = Adw.PasswordEntryRow(title="Token")
        save = Gtk.Button(label="Save", halign=Gtk.Align.CENTER)
        save.add_css_class("pill")
        save.connect("clicked", lambda _b: self.save_duckdns())
        duckdns.add(self.duckdns_entry)
        duckdns.add(self.token_entry)
        self.token_hint = Gtk.Label(xalign=0, wrap=True, margin_top=6)
        self.token_hint.add_css_class("dim-label")
        self.token_hint.add_css_class("caption")
        duckdns.add(self.token_hint)
        duckdns_save = Adw.PreferencesGroup()
        duckdns_save.add(save)

        for group in (name, pools, folder, apply, duckdns, duckdns_save):
            page.add(group)
        for entry in (self.host_entry, self.build_entry, self.game_entry, self.region_entry):
            entry.connect("changed", lambda *_: self.check_unapplied())
        box = Gtk.Box(orientation=Gtk.Orientation.VERTICAL)
        box.append(self.public_banner)
        box.append(page)
        page.set_vexpand(True)
        return box

    def load_settings(self):
        """Fills the Public name and Play pages from the files."""
        settings = current_settings()
        self.game_dir = settings["GAME_DIR"]
        self.host_entry.set_text(public_host())
        self.build_entry.set_text(settings["BUILD"])
        self.game_entry.set_text(settings["GAME"])
        self.region_entry.set_text(settings["REGION"])
        self.show_folder()
        duckdns = read_env(DUCKDNS_ENV)
        domain = unquote(duckdns.get("DUCKDNS_DOMAIN", ""))
        self.duckdns_entry.set_text(domain[:-len(".duckdns.org")]
                                    if domain.endswith(".duckdns.org") else domain)
        self.token_entry.set_text("")
        self.token_hint.set_label(
            "A token is saved. Leave Token empty to keep it, or type a new one to replace it."
            if unquote(duckdns.get("DUCKDNS_TOKEN", "")) else "No token saved yet.")
        self.show_profiles()
        self.check_unapplied()

    def show_folder(self):
        if not self.game_dir:
            self.folder_row.set_subtitle("Not chosen yet")
            return
        ok = os.path.isdir(os.path.join(self.game_dir, "data", "online"))
        self.folder_row.set_subtitle(self.game_dir + ("" if ok else "  (no data/online folder here)"))

    def choose_folder(self):
        dialog = Gtk.FileDialog(title="The game folder you share")
        if os.path.isdir(self.game_dir):
            dialog.set_initial_folder(Gio.File.new_for_path(self.game_dir))

        def chosen(_dialog, result):
            try:
                folder = dialog.select_folder_finish(result)
            except GLib.Error:
                return  # cancelled
            path = folder.get_path() if folder else None
            if not path:
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
        shared = default_profile(shared_profile_path(self.game_dir)) or {}
        changes = []
        if self.host_entry.get_text().strip() != shared.get("host", ""):
            changes.append("public name")
        for key, entry in (("build", self.build_entry), ("game", self.game_entry),
                           ("region", self.region_entry)):
            if entry.get_text().strip() != shared.get(key, ""):
                changes.append({"game": "game id"}.get(key, key))
        if self.game_dir != current_settings()["GAME_DIR"]:
            changes.append("game folder")
        return changes

    def check_unapplied(self):
        changes = self.pending()
        if changes:
            self.public_banner.set_title("Not applied yet: " + ", ".join(changes))
        self.public_banner.set_revealed(bool(changes))

    def ask_apply(self):
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
        if not os.path.isdir(os.path.join(self.game_dir, "data", "online")):
            self.toast("Choose the game folder: it has no data/online folder")
            return
        shared = default_profile(shared_profile_path(self.game_dir)) or {}
        build_note = ""
        if shared.get("build") and shared["build"] != values["BUILD"]:
            build_note = (f"\n\nThe build changes from {shared['build']} to {values['BUILD']}: "
                          "copies with the old build are no longer paired with the new ones.")
        self.confirm(
            "Apply these settings?",
            f"This writes {host}, build {values['BUILD']}, game id {values['GAME']} and region "
            f"{values['REGION']} into the game folder's data/online/servers.json and into "
            "host-profile.json, gives the server its new public address, and restarts Caddy, "
            "which then gets a certificate for the name if it has none."
            f"{build_note}\n\nPlayers only get the change in a new copy of the game folder: "
            "rebuild the zips (package-game-zip.py) and share them again.",
            "Apply", lambda: self.apply(host, values))

    def apply(self, host, values):
        # settings.env first: set-public-host.sh reads it.
        try:
            update_env(SETTINGS, dict(values, GAME_DIR=self.game_dir))
        except OSError as e:
            self.report("Could not save settings.env", str(e))
            return

        def busy(b):
            self.apply_button.set_sensitive(not b)
            self.apply_button.set_label("Applying…" if b else "Apply")

        def done(code, out):
            if code != 0:
                self.report("set-public-host.sh failed", out)
            else:
                self.toast("Applied. Rebuild the zips before you share the game folder.")
            self.load_settings()
        self.command([SET_PUBLIC_HOST_SH, host, self.game_dir], done, busy=busy, timeout=180)

    def save_duckdns(self):
        domain = self.duckdns_entry.get_text().strip()
        if domain.endswith(".duckdns.org"):
            domain = domain[:-len(".duckdns.org")]
        token = self.token_entry.get_text().strip()
        saved = read_env(DUCKDNS_ENV)
        if not token:
            token = unquote(saved.get("DUCKDNS_TOKEN", ""))
        if not domain or not DUCKDNS_RE.match(domain):
            self.toast("The DuckDNS name: letters, digits, dots and dashes")
            return
        if not token or not DUCKDNS_RE.match(token):
            self.toast("The token: letters, digits, dots and dashes, as duckdns.org shows it")
            return
        text = ("# The DuckDNS name and token (ddns/duckdns-update.sh). Keep this file\n"
                "# readable by you alone: the token lets anyone move the name.\n"
                f"DUCKDNS_DOMAIN={domain}\nDUCKDNS_TOKEN={token}\n")
        try:
            os.makedirs(os.path.dirname(DUCKDNS_ENV), exist_ok=True)
            write_file(DUCKDNS_ENV, text, 0o600)
        except OSError as e:
            self.report("Could not save ddns/duckdns.env", str(e))
            return
        self.token_entry.set_text("")
        self.load_settings()
        self.toast("Saved. Updating DuckDNS…")
        self.update_duckdns()

    # Play page -----------------------------------------------------------------

    def play_page(self):
        page = Adw.PreferencesPage()
        intro = Adw.PreferencesGroup(
            title="Play on this PC",
            description="Starts the game from your game folder signed in to the server on this "
                        "PC directly (127.0.0.1, no TLS), with host-profile.json. Your router "
                        "cannot reach its own public address from inside your network, so the "
                        "public name does not work here. Your account, matches and lobbies are "
                        "the same as everyone else's.")
        self.profile_row = Adw.ActionRow(title="This PC's profile (host-profile.json)",
                                         subtitle="…")
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
        shared = default_profile(shared_profile_path(current_settings()["GAME_DIR"]))
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
            self.address_row.set_subtitle("No data/online/servers.json in the game folder")
            self.key_row.set_subtitle("")
            self.key_row.copy_value = ""
            self.pools_row.set_subtitle("")
        self.play_folder_row.set_subtitle(current_settings()["GAME_DIR"])
        mine = default_profile(HOST_PROFILE)
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
        if not current_settings()["GAME_DIR"]:
            return "Choose the game folder on the Public name page, and apply."
        if not mine:
            return "There is no host-profile.json: apply the public name once to make it."
        if not shared:
            return "The game folder has no data/online/servers.json."
        if mine.get("build") != shared.get("build") or mine.get("game") != shared.get("game"):
            return (f"It is for build {mine.get('build')}, the game folder for "
                    f"{shared.get('build')}: apply the public name again.")
        if not os.path.exists(os.path.join(current_settings()["GAME_DIR"], "Ikemen_GO_Linux")):
            return "The game folder has no Ikemen_GO_Linux."
        return ""

    def ask_play(self):
        problem = self.play_problem(default_profile(HOST_PROFILE),
                                    default_profile(shared_profile_path(current_settings()["GAME_DIR"])))
        if problem:
            self.toast(problem)
            return
        if not self.server_running:
            self.confirm("The game server is not running",
                         "The game starts, but cannot sign in until you start the server.",
                         "Play anyway", self.play)
            return
        self.play()

    def play(self):
        game_dir = current_settings()["GAME_DIR"]
        log_dir = os.path.join(GLib.get_user_cache_dir(), "server-control", SERVER_ID)
        os.makedirs(log_dir, exist_ok=True)
        log = os.path.join(log_dir, "play-on-this-pc.log")
        try:
            with open(log, "w", encoding="utf-8") as out:
                game = subprocess.Popen([PLAY_SH, game_dir], stdin=subprocess.DEVNULL,
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

    def logs_page(self):
        self.log_sources = [
            ("Game server", lambda: run(JOURNALCTL + ["-u", SERVER_UNIT, "-n", str(LOG_LINES),
                                                      "--no-pager", "-q", "-o", "short"],
                                        timeout=15)[1]),
            ("Nakama's own log", lambda: tail(NAKAMA_LOG, LOG_LINES)),
            ("HTTPS (Caddy)", lambda: run(JOURNALCTL + ["-u", CADDY_UNIT, "-n", str(LOG_LINES),
                                                        "--no-pager", "-q", "-o", "short"],
                                          timeout=15)[1]),
            ("DuckDNS", lambda: run(JOURNALCTL + ["-u", DUCKDNS_UNIT, "-n", str(LOG_LINES),
                                                  "--no-pager", "-q", "-o", "short"],
                                    timeout=15)[1]),
        ]
        bar = Gtk.Box(spacing=12, margin_top=12, margin_bottom=6, margin_start=12, margin_end=12)
        self.log_choice = Gtk.DropDown.new_from_strings([name for name, _ in self.log_sources])
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
        if self.log_loading:
            return
        self.log_loading = True
        index = self.log_choice.get_selected()
        source = self.log_sources[index][1]

        def done(text):
            self.log_loading = False
            if index != self.log_choice.get_selected():
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
        in_background(source, done)

    def page_changed(self):
        # The Public name page keeps what was typed until Apply.
        if self.stack.get_visible_child_name() == "logs":
            self.load_log()
        elif self.stack.get_visible_child_name() == "play":
            self.show_profiles()

    # Status ------------------------------------------------------------------

    def tick(self):
        self.refresh()
        if self.stack.get_visible_child_name() == "logs" and self.follow.get_active():
            self.load_log()
        return True

    def refresh(self):
        """Reads the services' states and health in the background."""
        if self.refreshing:
            self.refresh_again = True  # after this one: something just changed
            return
        self.refreshing = True
        self.refresh_again = False
        host = public_host()
        nakama_port = self.nakama_port

        def work():
            units, error = unit_states()
            health = {"nakama": check_nakama(nakama_port) if nakama_port
                      else (False, "checking")}
            if host:
                health["https"] = check_https(host, caddy_https_port())
            return units, error, health, host

        def done(result):
            self.refreshing = False
            self.show_status(*result)
            if self.refresh_again:
                self.refresh()
        in_background(work, done)

    def show_status(self, units, error, health, host):
        self.units = units
        server = units.get(SERVER_UNIT, {})
        caddy = units.get(CADDY_UNIT, {})
        duck = units.get(DUCKDNS_UNIT, {})
        timer = units.get(DUCKDNS_TIMER, {})
        problems = []

        self.server_running = server.get("ActiveState") in ("active", "activating", "reloading")
        nakama_ok, nakama_msg = health["nakama"]
        state, text = self.describe(server)
        if server.get("ActiveState") == "active":
            if nakama_ok:
                text += " · answers on this PC"
            elif nakama_msg == "checking":
                text += " · checking…"
            else:
                state, text = "warning", text + f" · not answering yet ({nakama_msg})"
                problems.append("the game server is not answering")
        elif state == "error":
            problems.append("the game server failed")
        elif not self.server_running:
            problems.append("the game server is stopped")
        self.server_row.show(state, text, self.server_running)

        caddy_running = caddy.get("ActiveState") in ("active", "activating", "reloading")
        state, text = self.describe(caddy)
        if not host:
            state, text = "warning", "Waiting for a public name (Public name page)"
            problems.append("there is no public name")
        elif caddy.get("ActiveState") == "active":
            https_ok, https_msg = health.get("https", (False, ""))
            if https_ok:
                text += f" · https://{host} answers"
            else:
                state, text = "warning", text + f" · {host} does not answer yet ({https_msg})"
                problems.append("HTTPS is not answering")
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

        boot = all(units.get(u, {}).get("UnitFileState") == "enabled" for u in BOOT_UNITS)
        self.boot_row.handler_block(self.boot_handler)
        self.boot_row.set_active(boot)
        self.boot_row.handler_unblock(self.boot_handler)

        if server.get("LoadState") == "not-found":
            problems = ["its services are not installed (./install-services.sh, README.md)"]
        if error:
            self.banner.set_title("systemctl did not answer: " + error.splitlines()[-1][:120])
        elif problems:
            self.banner.set_title("Players cannot play right now: " + "; ".join(problems))
        self.banner.set_revealed(bool(error or problems))
        self.show_profiles()

    def describe(self, unit):
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
        if not active:
            return "error", "Unknown: systemctl did not report it"
        return "off", "Stopped"

    def show_duckdns(self, state, text):
        self.duckdns_row.set_subtitle(text)
        self.duckdns_icon.set_from_icon_name({"ok": "object-select-symbolic",
                                              "warning": "dialog-warning-symbolic",
                                              "error": "dialog-error-symbolic"}[state])
        for css in ("success", "warning", "error"):
            self.duckdns_icon.remove_css_class(css)
        self.duckdns_icon.add_css_class({"ok": "success", "warning": "warning",
                                         "error": "error"}[state])


class SetupWindow(Adw.ApplicationWindow):
    """Shown while settings.env names no server."""

    def __init__(self, app):
        super().__init__(application=app, title=TITLE, default_width=560, default_height=420)
        view = Adw.ToolbarView()
        view.add_top_bar(Adw.HeaderBar())
        view.set_content(Adw.StatusPage(
            icon_name="network-server-symbolic", title="Set up the server first",
            description=f"Copy settings.env.example to settings.env in {HERE}, set SERVER_ID "
                        "and the rest, then create the server and install its services: "
                        "README.md, next to this program, says how."))
        self.set_content(view)


class App(Adw.Application):
    def __init__(self):
        super().__init__(application_id=APP_ID, flags=Gio.ApplicationFlags.DEFAULT_FLAGS)

    def do_activate(self):
        window = self.get_active_window() or (Window(self) if SERVER_ID else SetupWindow(self))
        window.present()


if __name__ == "__main__":
    raise SystemExit(App().run(None))
