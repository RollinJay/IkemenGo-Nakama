#!/usr/bin/env python3
"""Builds the zip you share with your players from the game folder.

    python3 package-game-zip.py [game folder] [zip file]
    python3 package-game-zip.py --android <ikemen-go.apk> [game folder] [zip file]

The game folder is settings.env's GAME_DIR unless given. Without --android:
dist/<build>.zip, the game for Windows, Linux and macOS, which unpacks to a
folder named after the build in the folder's data/online/servers.json.

With --android: dist/<build>-Android.zip, holding the fork's Android app (the
APK given), the game folder without the desktop programs and libraries (whose
lib/ would collide with the app's), and the steps to install it. On its first
start the app asks for a game folder and runs the game from it; it copies its
own files over that folder when its external/script files differ from the
folder's, so the zip is refused for an app whose scripts, or engine, are not
the game folder's.

Both leave out this PC's own online account and other per-PC data, and refuse
to build while the server profile does not name the public server:
- save/nakama_device_id is the sign-in of this copy of the game. Shared, every
  player would sign in as the same account (yours).
- save/ranked_pending.json, save/stats.json, logs, replays, the CBR AI's
  learned data, the fork's launcher's server files and the Vulkan shader cache
  belong to this PC; nakama/ holds the server's modules and documents.
More to leave out goes in zip-exclude.txt next to this program, one per line:
a file (data/old.def), a folder ending in / (chars/old/), or a pattern
(*.psd).
"""
import fnmatch
import json
import os
import re
import sys
import time
import zipfile

HERE = os.path.dirname(os.path.realpath(__file__))

# Paths relative to the game folder, with "/" separators.
EXCLUDED_FILES = {
    "save/nakama_device_id",
    "save/ranked_pending.json",
    "save/stats.json",
    # Ikemen_GO -server-wizard's and -server's files
    "save/server.json",
    "save/server-client.json",
    "save/server-nakama.yml",
    "save/server-nakama-database.yml",
}
EXCLUDED_DIRS = {"save/logs", "save/replays", "save/cbr", "cache", "nakama"}
# The desktop programs, which the Android app replaces.
DESKTOP_FILES = {"Ikemen_GO.exe", "Ikemen_GO_Linux", "Ikemen_GO.command",
                 "Ikemen_GO.desktop", "ikemen-go.png"}
DESKTOP_DIRS = {"lib", "I.K.E.M.E.N-Go.app"}
# Kept as empty folders, as the release builds have them.
KEPT_EMPTY_DIRS = {"save/logs", "save/replays"}
# Launchers that must stay executable whatever their mode here.
EXECUTABLES = {"Ikemen_GO_Linux", "Ikemen_GO.command"}
JUNK = {".DS_Store", "Thumbs.db", "desktop.ini", "__pycache__"}

ANDROID_README = """\
{title} ({build}) for Android
Needs Android 14 or newer, on a 64-bit (arm64) phone or tablet.

1. Put the {build} folder on the device: unzip this file on it (for example
   into Download), or copy the folder over from a computer.
2. Install ikemen-go.apk. Android asks to allow installing apps from your
   file manager or browser: allow it. If another Ikemen GO app is installed,
   uninstall it first.
3. Open Ikemen GO. Android shows "All files access": turn it on for
   Ikemen GO, then go back.
4. In the folder picker, open the {build} folder, tap "Use this folder",
   then "Allow". The game starts from that folder.

To pick another folder later, tap "Select Game Folder" at the top right in
the first 5 seconds after the app starts.
"""


def read_env(path):
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


def extra_excludes():
    try:
        with open(os.path.join(HERE, "zip-exclude.txt"), encoding="utf-8") as f:
            return [l.strip() for l in f if l.strip() and not l.lstrip().startswith("#")]
    except OSError:
        return []


def default_server(game_dir):
    path = os.path.join(game_dir, "data", "online", "servers.json")
    with open(path, encoding="utf-8") as f:
        profiles = json.load(f)
    default = profiles.get("default")
    servers = {s.get("id"): s for s in profiles.get("servers", [])}
    server = servers.get(default)
    if server is None:
        sys.exit(f"{path}: no profile named by default ({default!r})")
    host = str(server.get("host", "")).strip().lower()
    if (not host or host.endswith(".invalid") or host.endswith("example.com")
            or host.endswith("example.org") or host in ("localhost", "127.0.0.1", "::1")):
        sys.exit(f"{path}: the server profile names {host!r}, not the public "
                 "server; run set-public-host.sh first")
    # The name Caddy serves, when this folder's Caddy is set up.
    served = [v.strip() for k, v in read_env(os.path.join(HERE, "caddy", "public-host.env")).items()
              if k == "PUBLIC_HOST" or k.endswith("_PUBLIC_HOST")]
    if served and served[-1].lower() != host:
        sys.exit(f"{path} names {host!r} but Caddy serves {served[-1]!r}: "
                 "run set-public-host.sh again")
    build = str(server.get("build", ""))
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", build):
        sys.exit(f"{path}: the build {build!r} cannot name the zip")
    return server


def check_config(game_dir):
    """Join Game addresses saved in save/config.ini are this PC's friends'."""
    path = os.path.join(game_dir, "save", "config.ini")
    if not os.path.exists(path):
        return
    with open(path, encoding="utf-8", errors="replace") as f:
        saved = [l.strip() for l in f
                 if re.match(r"\s*IP\.", l) and not re.match(r"\s*IP\.localhost\s*=", l)]
    if saved:
        sys.exit(f"{path} has saved Join Game addresses ({', '.join(saved)}); "
                 "remove those lines first")


def engine_revision(data):
    m = re.search(rb"vcs\.revision=([0-9a-f]{40})", data)
    return m.group(1).decode() if m else None


def check_apk(game_dir, path):
    """The app must not overwrite the game folder, and must play PC players."""
    if not os.path.isfile(path):
        sys.exit(f"no APK at {path}")
    with zipfile.ZipFile(path) as apk:
        scripts = [n for n in apk.namelist()
                   if n.startswith("assets/external/script/") and not n.endswith("/")]
        for n in scripts:
            local = os.path.join(game_dir, n[len("assets/"):])
            if not os.path.isfile(local) or open(local, "rb").read() != apk.read(n):
                sys.exit(f"the APK's {n[len('assets/'):]} differs from the game folder's: "
                         "on its first start the app would copy its own files over the game")
        apk_rev = engine_revision(apk.read("lib/arm64-v8a/libmain.so"))
    with open(os.path.join(game_dir, "Ikemen_GO_Linux"), "rb") as f:
        pc_rev = engine_revision(f.read())
    if not apk_rev or apk_rev != pc_rev:
        sys.exit(f"the APK is engine {apk_rev}, the desktop programs {pc_rev}: "
                 "use the APK of the same release")


def excluded(rel, rel_root, name, android, patterns):
    if rel in EXCLUDED_FILES or any(fnmatch.fnmatch(rel, p) or fnmatch.fnmatch(name, p)
                                    for p in patterns if not p.endswith("/")):
        return True
    return android and not rel_root and name in DESKTOP_FILES


def excluded_dir(rel, android, patterns):
    if rel in EXCLUDED_DIRS or (android and rel in DESKTOP_DIRS):
        return True
    return any(p.endswith("/") and fnmatch.fnmatch(rel + "/", p) for p in patterns)


def main():
    args = sys.argv[1:]
    apk = None
    if "--android" in args:
        i = args.index("--android")
        if i + 1 >= len(args):
            sys.exit("--android needs the APK: --android <ikemen-go.apk>")
        apk = os.path.realpath(args[i + 1])
        del args[i:i + 2]
    settings = read_env(os.path.join(HERE, "settings.env"))
    game_dir = args[0] if args else settings.get("GAME_DIR", "")
    if not game_dir:
        sys.exit("no game folder: set GAME_DIR= in settings.env, or give it as the argument")
    game_dir = os.path.realpath(game_dir)
    server = default_server(game_dir)
    check_config(game_dir)
    android = apk is not None
    if android:
        check_apk(game_dir, apk)
    build = server["build"]
    name = build + ("-Android" if android else "")
    out = os.path.realpath(args[1] if len(args) > 1 else os.path.join(HERE, "dist", name + ".zip"))
    if os.path.commonpath([out, game_dir]) == game_dir:
        sys.exit("write the zip outside the game folder")
    # The game folder inside the zip.
    top = f"{name}/{build}" if android else build
    patterns = extra_excludes()

    os.makedirs(os.path.dirname(out), exist_ok=True)
    tmp = out + ".part"
    left_out = []
    count = 0
    try:
        with zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED, compresslevel=6,
                             strict_timestamps=False) as z:
            for root, dirs, files in os.walk(game_dir):
                rel_root = os.path.relpath(root, game_dir).replace(os.sep, "/")
                rel_root = "" if rel_root == "." else rel_root
                kept = []
                for d in sorted(dirs):
                    rel = f"{rel_root}/{d}" if rel_root else d
                    if d in JUNK:
                        continue
                    if excluded_dir(rel, android, patterns):
                        left_out.append(rel + "/")
                        if rel in KEPT_EMPTY_DIRS:
                            folder = zipfile.ZipInfo(f"{top}/{rel}/")
                            # drwxr-xr-x, and the MS-DOS directory flag
                            folder.external_attr = (0o40755 << 16) | 0x10
                            z.writestr(folder, b"")
                        continue
                    if os.path.islink(os.path.join(root, d)):
                        sys.exit(f"{rel} is a link to a folder: replace it with the folder itself")
                    kept.append(d)
                dirs[:] = kept
                for f in sorted(files):
                    rel = f"{rel_root}/{f}" if rel_root else f
                    full = os.path.join(root, f)
                    if f in JUNK:
                        continue
                    if excluded(rel, rel_root, f, android, patterns):
                        left_out.append(rel)
                        continue
                    if not os.path.isfile(full):
                        continue
                    # Keeps each file's mode, so Ikemen_GO_Linux stays
                    # executable. A symbolic link to a file is stored as the
                    # file it points to, which Windows' extractor handles.
                    info = zipfile.ZipInfo.from_file(full, f"{top}/{rel}",
                                                     strict_timestamps=False)
                    if rel in EXECUTABLES or "/Contents/MacOS/" in f"/{rel}":
                        info.external_attr |= 0o755 << 16
                    info.compress_type = zipfile.ZIP_DEFLATED
                    with open(full, "rb") as src, z.open(info, "w") as dst:
                        while chunk := src.read(1 << 20):
                            dst.write(chunk)
                    count += 1
            if android:
                z.write(apk, f"{name}/ikemen-go.apk")
                readme = zipfile.ZipInfo(f"{name}/READ ME - ANDROID.txt", time.localtime()[:6])
                readme.external_attr = 0o644 << 16
                title = settings.get("TITLE", "").strip() or build
                z.writestr(readme, ANDROID_README.format(title=title, build=build)
                           .replace("\n", "\r\n"))
                count += 2
        os.replace(tmp, out)
    finally:
        if os.path.exists(tmp):
            os.remove(tmp)

    print(f"Wrote {out}: {count} files, {os.path.getsize(out) / 1048576:.0f} MB")
    print(f"Server: {server.get('host')}:{server.get('port')} "
          f"(TLS {'on' if server.get('ssl') else 'off'}), "
          f"game {server.get('game')}, build {server.get('build')}")
    print(f"Left out {len(left_out)}: " + (", ".join(left_out) if left_out else "nothing"))


if __name__ == "__main__":
    main()
