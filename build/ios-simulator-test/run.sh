#!/usr/bin/env bash
# Runs the iOS app in an iOS Simulator and screenshots it, for CI
# (.github/workflows/ios-simulator.yml); it also works on a Mac with Xcode.
#
#   build/ios-simulator-test/run.sh <app> <default-assets-dir> <device> <out-dir>
#
#   app                 the simulator .app (./build/build.sh iOSSimulator puts it in
#                       bin/ios-simulator/I.K.E.M.E.N-Go.app)
#   default-assets-dir  the files staged for DefaultAssets.zip
#                       (build/ios-sim-app/ikemen-ios/DefaultAssets)
#   device              iPhone or iPad: the newest available simulator of that kind
#                       on the newest iOS runtime
#   out-dir             screenshots/, logs/ and result.txt go here
#
# The app gets a fresh test game folder in its Documents (IKEMEN_TEST_GAME_FOLDER,
# a test hook that only simulator builds have) holding only the test's files:
#   - external/mods/zz_ios_ci.lua, which walks the menus and logs each step;
#   - data/ikemen1/system_ci.def, the default screenpack with HDBZ-like
#     [Touch Controls] (grid layout, no D and W buttons, Start in D's place);
#   - save/config.ini choosing that screenpack.
# The app installs the engine's default assets (DefaultAssets.zip) around them on
# its first run, as it does on a device, keeping the test's files.
#
# Exit status: 0 when the title screen came up and the app did not crash; 1
# otherwise. Missing later steps are reported as warnings. STRICT=1 fails on
# them too.
set -uo pipefail

app="${1:?app path}"
assets="${2:?default assets dir}"
kind="${3:?iPhone or iPad}"
out="${4:?output dir}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

title_timeout="${TITLE_TIMEOUT:-420}" # s: first run unzips the assets, then a 5 s controller wait
step_timeout="${STEP_TIMEOUT:-120}"   # s, for each later step
settle="${SETTLE:-3}"                 # s to let the screen settle before a screenshot

mkdir -p "$out/screenshots" "$out/logs"
out="$(cd "$out" && pwd)"
result="$out/result.txt"
: > "$result"
note() { echo "$*"; echo "$*" >> "$result"; }
gh_warning() { echo "::warning::[$kind] $*"; echo "WARNING: $*" >> "$result"; }
gh_error() { echo "::error::[$kind] $*"; echo "ERROR: $*" >> "$result"; }

# --- Pick and boot a simulator ---
# The newest iOS runtime, and on it the last available device whose name starts
# with $kind (the device list goes from older to newer models).
read -r udid name < <(xcrun simctl list devices available -j | python3 -c '
import json, re, sys
kind = sys.argv[1]
devices = json.load(sys.stdin)["devices"]
def version(rt):
    m = re.search(r"iOS-(\d+)-(\d+)(?:-(\d+))?$", rt)
    return tuple(int(x or 0) for x in m.groups()) if m else None
runtimes = sorted((v, rt) for rt in devices if (v := version(rt)))
for _, rt in reversed(runtimes):
    found = [d for d in devices[rt] if d["name"].startswith(kind) and d.get("isAvailable", True)]
    if found:
        d = found[-1]
        print(d["udid"], d["name"].replace(" ", "_"))
        break
' "$kind")
if [[ -z "${udid:-}" ]]; then
	gh_error "no available $kind simulator"
	xcrun simctl list devices available
	exit 1
fi
note "device: $name ($udid)"

# A headless simulator has no hardware keyboard, but in case Simulator.app's
# setting applies: off, so that the on-screen keyboard shows for a text box.
defaults write com.apple.iphonesimulator ConnectHardwareKeyboard -bool false || true

xcrun simctl shutdown "$udid" >/dev/null 2>&1 || true
xcrun simctl erase "$udid" || true
xcrun simctl boot "$udid"
xcrun simctl bootstatus "$udid" -b || { gh_error "simulator did not boot"; exit 1; }

# --- Install the app and prepare its test game folder ---
bundle="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Info.plist")"
executable="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$app/Info.plist")"
xcrun simctl install "$udid" "$app" || { gh_error "simctl install failed"; exit 1; }
data="$(xcrun simctl get_app_container "$udid" "$bundle" data)"
game="$data/Documents/ci-game"
rm -rf "$game"
mkdir -p "$game/ci" "$game/external/mods" "$game/data/ikemen1" "$game/save"
cp "$here/zz_ios_ci.lua" "$game/external/mods/"

# The default screenpack (data/ikemen1/system.def) with the test's sections in
# place of its own: [Touch Controls] as HDBZ sets them, and no attract demo,
# which would start a fight while the test waits at the title screen.
python3 - "$assets/data/ikemen1/system.def" "$game/data/ikemen1/system_ci.def" <<'EOF'
import re, sys
src, dst = sys.argv[1], sys.argv[2]
text = open(src, encoding="utf-8", errors="surrogateescape").read()
ours = {
    "touch controls": "[Touch Controls]\nlayout = grid\nbuttons = a, b, c, x, y, z, s, m\nstart.slot = d\n",
    "demo mode": "[Demo Mode]\nenabled = 0\n",
}
lines, skip = [], False
for line in text.splitlines(keepends=True):
    m = re.match(r"\s*\[([^\]]+)\]", line)
    if m:
        skip = m.group(1).strip().lower() in ours
    if not skip:
        lines.append(line)
body = "".join(lines).rstrip("\r\n") + "\n\n; iOS Simulator test (build/ios-simulator-test/run.sh)\n"
body += "\n".join(ours.values())
open(dst, "w", encoding="utf-8", errors="surrogateescape").write(body)
EOF
[[ -s "$game/data/ikemen1/system_ci.def" ]] || { gh_error "could not write the test screenpack"; exit 1; }
printf '[Config]\nMotif = data/ikemen1/system_ci.def\n' > "$game/save/config.ini"

# --- Launch ---
# The app's unified log (NSLog) for the whole run.
xcrun simctl spawn "$udid" log stream --style compact --level debug \
	--predicate "process == \"$executable\"" > "$out/logs/device-log.txt" 2>&1 &
log_pid=$!
started="$(date +%s)"
touch "$out/logs/.started" # crash reports newer than this are this run's
sleep 2

launch="$(SIMCTL_CHILD_IKEMEN_TEST_GAME_FOLDER="$game" \
	SIMCTL_CHILD_IKEMEN_TEST_KEEP_TOUCH_CONTROLS=1 \
	xcrun simctl launch --terminate-running-process \
		--stdout="$out/logs/game-stdout.txt" --stderr="$out/logs/game-stderr.txt" \
		"$udid" "$bundle")"
echo "$launch"
pid="${launch##*: }"
if ! [[ "$pid" =~ ^[0-9]+$ ]]; then
	gh_error "simctl launch gave no process ID: $launch"
	pid=""
fi

shot() {
	xcrun simctl io "$udid" screenshot "$out/screenshots/$1.png" >/dev/null 2>&1 \
		&& note "screenshot: $1.png" || gh_warning "screenshot $1 failed"
}

# The simulator's apps are processes of this Mac, so kill -0 sees them.
alive() { [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; }

# Waits for "STEP <name>" from the mod. 0: reached; 1: timed out; 2: the app is gone.
wait_step() {
	local step="$1" limit="$2" t=0
	while (( t < limit )); do
		grep -qx "STEP $step" "$game/ci/steps.log" 2>/dev/null && return 0
		alive || return 2
		sleep 1
		t=$((t + 1))
	done
	return 1
}

failed=0
crashed=0
step_done() { touch "$game/ci/ack-$1"; }

# Title screen
wait_step title "$title_timeout"
case $? in
	0)
		note "title screen after $(( $(date +%s) - started )) s"
		sleep "$settle"
		shot 01-title
		# The touch controls stay shown (IKEMEN_TEST_KEEP_TOUCH_CONTROLS); a
		# second shot, once the game has sent its button set (0x8003).
		sleep "$settle"
		shot 02-touch-controls
		step_done title
		;;
	1) gh_error "no title screen within $title_timeout s"; shot 01-no-title; failed=1 ;;
	2) gh_error "the app exited before the title screen"; crashed=1; failed=1 ;;
esac

# Text box with the on-screen keyboard, then Options > Input Settings > On-screen Buttons
if (( ! failed )); then
	for step in textbox touchbuttons; do
		wait_step "$step" "$step_timeout"
		case $? in
			0)
				note "step $step reached"
				# The keyboard slides in.
				sleep "$settle"
				[[ "$step" == textbox ]] && shot 03-textbox-keyboard || shot 04-options-onscreen-buttons
				step_done "$step"
				;;
			1) gh_warning "step $step not reached within $step_timeout s"; shot "missing-$step"; [[ "${STRICT:-0}" == 1 ]] && failed=1; break ;;
			2) gh_error "the app exited at step $step"; crashed=1; failed=1; break ;;
		esac
	done
	if ! grep -qx DONE "$game/ci/steps.log" 2>/dev/null; then
		sleep 2
	fi
	grep -qx DONE "$game/ci/steps.log" 2>/dev/null && note "all steps done"
fi

# Still running at the end?
if (( ! crashed )) && [[ -n "$pid" ]] && ! alive; then
	gh_error "the app is no longer running"
	crashed=1
	failed=1
fi
shot 99-last

# --- Collect ---
xcrun simctl terminate "$udid" "$bundle" >/dev/null 2>&1 || true
sleep 2
kill "$log_pid" 2>/dev/null || true
cp "$game/ci/steps.log" "$out/logs/steps.txt" 2>/dev/null || true
cp "$game/save/config.ini" "$out/logs/config.ini" 2>/dev/null || true
cp "$game/data/ikemen1/system_ci.def" "$out/logs/system_ci.def" 2>/dev/null || true
[[ -d "$game/save/logs" ]] && cp -R "$game/save/logs" "$out/logs/game-save-logs" 2>/dev/null || true
# Crash reports of simulator apps land in the Mac's own DiagnosticReports.
mkdir -p "$out/logs/crash-reports"
find "$HOME/Library/Logs/DiagnosticReports" -maxdepth 1 -newer "$out/logs/.started" \
	\( -name "${executable}*" -o -name "*${executable}*" \) -exec cp {} "$out/logs/crash-reports/" \; 2>/dev/null || true
if compgen -G "$out/logs/crash-reports/*" >/dev/null; then
	gh_error "crash report(s): $(cd "$out/logs/crash-reports" && ls | tr '\n' ' ')"
	failed=1
fi
grep -h '^FAIL' "$game/ci/steps.log" 2>/dev/null | while read -r line; do gh_warning "mod: $line"; done

xcrun simctl shutdown "$udid" >/dev/null 2>&1 || true
note "result: $([[ $failed == 0 ]] && echo pass || echo FAIL)"
exit "$failed"
