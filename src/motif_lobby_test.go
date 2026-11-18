package main

import (
	"os"
	"reflect"
	"testing"

	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
	"gopkg.in/ini.v1"
)

// defaultLobbyInfo reads [Lobby Info] from the built-in motif as loadMotif
// assigns its keys (the defaults of the struct tags are not applied here,
// so every value checked below is written in defaultMotif.ini itself).
func defaultLobbyInfo(t *testing.T) LobbyInfoProperties {
	t.Helper()
	f, err := LoadINIText(preprocessINIContent(NormalizeNewlines(string(defaultMotif))), ini.LoadOptions{
		InsensitiveSections: true, SkipUnrecognizableLines: true, PreserveSurroundedQuote: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sec, err := f.GetSection("Lobby Info")
	if err != nil {
		t.Fatal(err)
	}
	var m Motif
	for _, key := range sec.Keys() {
		if err := assignField(&m, parseQueryPath("lobby_info."+key.Name()), key.Value(), ""); err != nil {
			t.Fatalf("%s: %v", key.Name(), err)
		}
	}
	return m.LobbyInfo
}

// The default room keeps half the game size free on the right for the
// match window: at 640x480 (HDBZ) the match plays at 320x240.
func TestLobbyWatchWindowDefaults(t *testing.T) {
	r := defaultLobbyInfo(t).Room
	w := r.Watch
	if got, want := w.Window, [4]float32{156, 48, 160, 120}; got != want {
		t.Fatalf("room.watch.window = %v, want %v", got, want)
	}
	if w.Window[0]+w.Window[2] > 320 || w.Window[1]+w.Window[3] > 240 {
		t.Errorf("window %v leaves the 320x240 screen", w.Window)
	}
	if w.Volume != 50 {
		t.Errorf("room.watch.volume = %d, want 50", w.Volume)
	}
	if !w.Frame.Visible || !w.Bg.Visible {
		t.Errorf("frame/bg not visible: %+v %+v", w.Frame, w.Bg)
	}
	for _, key := range []string{"waiting", "loading"} {
		if w.Text.Text[key] == "" {
			t.Errorf("room.watch.text.text.%s is empty", key)
		}
	}
	for _, key := range []string{"p2p", "server"} {
		if w.Transport.Text[key] == "" {
			t.Errorf("room.watch.transport.text.%s is empty", key)
		}
	}
}

// The watching layout: the members in one column left of the window and
// the menu as a bar below it, moved with left/right.
func TestLobbyWatchingLayoutDefaults(t *testing.T) {
	r := defaultLobbyInfo(t).Room
	sl := r.Watching.Slots
	if sl.Columns != 1 {
		t.Errorf("room.watching.slots.columns = %d, want 1", sl.Columns)
	}
	// The column's boxes end left of the window.
	right := sl.Pos[0] + float32(sl.Bg["member"].Box.Coords[2])
	if right >= r.Watch.Window[0] {
		t.Errorf("member column reaches x=%v, the window starts at %v", right, r.Watch.Window[0])
	}
	// Eight rows fit above the menu bar.
	if bottom := sl.Pos[1] + 7*sl.Spacing[1] + float32(sl.Bg["member"].Box.Coords[3]); bottom >= r.Watching.Menu.Pos[1]+float32(r.Watching.Menu.Box.Coords[1]) {
		t.Errorf("member column ends at y=%v, below the menu bar", bottom)
	}
	bar := r.Watching.Menu
	if !reflect.DeepEqual(bar.Next.Key, []string{"R"}) || !reflect.DeepEqual(bar.Previous.Key, []string{"L"}) {
		t.Errorf("bar keys next %q previous %q, want R and L", bar.Next.Key, bar.Previous.Key)
	}
	if bar.Spacing <= 0 || !bar.Box.Visible || !bar.Cursor.Visible {
		t.Errorf("bar spacing %v box %v cursor %v", bar.Spacing, bar.Box.Visible, bar.Cursor.Visible)
	}
	if bar.Pos[1] <= r.Watch.Window[1]+r.Watch.Window[3] {
		t.Errorf("bar at y=%v overlaps the window (ends at %v)", bar.Pos[1], r.Watch.Window[1]+r.Watch.Window[3])
	}
}

// WATCH AUTO / OFF is a room menu item with its two values.
func TestLobbyAutoWatchMenuItem(t *testing.T) {
	m := defaultLobbyInfo(t).Room.Menu
	if m.Itemname["autowatch"] == "" {
		t.Error("room.menu.itemname.autowatch is missing")
	}
	for _, key := range []string{"watchauto", "watchoff"} {
		if m.Valuename[key] == "" {
			t.Errorf("room.menu.valuename.%s is missing", key)
		}
	}
}

// lobby.lua must at least parse: the engine only finds a syntax error when
// the lobby is opened.
func TestLobbyScriptParses(t *testing.T) {
	for _, path := range []string{"../external/script/lobby.lua", "../external/script/lobby_match.lua"} {
		f, err := os.Open(path)
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		_, err = parse.Parse(f, path)
		f.Close()
		if err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// lobbyLuaStubs stands in for the engine and the nakama API around
// lobby.lua's room code. calls records what was called, in order; now is
// what os.clock returns.
const lobbyLuaStubs = `
calls = {}
local function rec(name, ret)
	return function(...)
		table.insert(calls, {name = name, args = {...}})
		return ret
	end
end
function count(name)
	local n = 0
	for _, c in ipairs(calls) do
		if c.name == name then
			n = n + 1
		end
	end
	return n
end
function lastIndex(name)
	local idx = 0
	for i, c in ipairs(calls) do
		if c.name == name then
			idx = i
		end
	end
	return idx
end
function check(cond, what)
	if not cond then
		error(what, 2)
	end
end
now = 100
os.clock = function() return now end
motif = {lobby_info = {
	room = {watch = {window = {156, 48, 160, 120}, volume = 50}},
	message = {},
	textinput = {confirm = {keycode = 1}, truncate = {keycode = 2}, trim = {keycode = 3}, paste = {keycode = 4}},
}}
online = {
	f_status = function() return {connected = true, match_id = 'm', rtt_ms = -1, connection = ''} end,
	f_clearMatchNames = function() end,
}
hook = {lists = {}, add = rec("hook.add"), stop = function(list, name) table.insert(calls, {name = "hook.stop", args = {list, name}}) hook.lists[list][name] = nil end}
setWatchWindow = rec('setWatchWindow')
clearWatchWindow = rec('clearWatchWindow')
stopWatching = rec('stopWatching')
watchActive = false
watchWindowActive = function() return watchActive end
resetTokenGuard = rec('resetTokenGuard')
resetKey = function() end
escDown = false
esc = function(v) if v == nil then return escDown end escDown = v end
keyDown = nil
getKey = function(k) return k == keyDown end
getKeyText = function() return '' end
exitReplay = rec('exitReplay')
setMatchNo = function() end
resetRemapInput = function() end
playBgm = function() end
vsLoading = true
gameOption = function(k)
	if k == 'Config.VsScreenLoading' then
		return vsLoading
	end
	return 0
end
modifyGameOption = rec('modifyGameOption')
jsonDecode = function() error('no prefs') end
jsonEncode = function() end
start = {f_game = function() return 1 end, f_selectReset = function() end, f_getOrderChars = function() return {} end}
main = {f_tableCopy = function(t) return t end, f_saveBaseRemapInput = function() end, f_default = function() end}
`

// lobbyLuaNakama is the nakama API as the room uses it, set after
// lobby.lua is loaded (which registers its callbacks only when nakama
// exists). state is what lobbyState returns once; stream what
// replayStatus returns.
const lobbyLuaNakama = `
state = nil
stream = nil
local pending = false
function setState(s)
	state = s
	pending = true
end
nakama = {
	userId = function() return 'me' end,
	lobbyState = function(v)
		if pending then
			pending = false
			return state, (v or 0) + 1
		end
		return nil, v
	end,
	lobbyEvents = function() return {} end,
	lobbySend = function(kind, t)
		table.insert(calls, {name = 'lobbySend', args = {kind, t}})
	end,
	ping = function() table.insert(calls, {name = "ping", args = {}}) end,
	replayStatus = function() return stream end,
	startWatchLink = function(host) table.insert(calls, {name = 'startWatchLink', args = {host}}) return true end,
	stopWatchLink = function() table.insert(calls, {name = 'stopWatchLink', args = {}}) end,
	startSpectatorReplay = function() table.insert(calls, {name = 'startSpectatorReplay', args = {}}) return true end,
}
lobby = dofile('../external/script/lobby.lua')
L = lobby.session
I = lobby.internal
function playing(seq, streamStarted)
	return {
		phase = 'playing', settings = {watch = 3}, members = {},
		current = {seq = seq, player1 = 'host', player2 = 'p2', watch = streamStarted},
	}
end
function readyStream(seq)
	return {
		ready = true, transport = 'server', stage = 'stages/a.def',
		match_info = {kind = 'lobby', seq = seq, selection = {p1 = {'a'}, p2 = {'b'}}, names = {'A', 'B'}},
	}
end
function newRoom()
	return {watch = {done = {}, asked = {}}, signature = nil}
end
`

// runLobbyLua loads lobby.lua with the stubs and runs body.
func runLobbyLua(t *testing.T, body string) {
	t.Helper()
	if _, err := os.Stat("../external/script/lobby.lua"); err != nil {
		t.Skip(err)
	}
	l := lua.NewState()
	defer l.Close()
	if err := l.DoString(lobbyLuaStubs + lobbyLuaNakama + body); err != nil {
		t.Fatal(err)
	}
}

// Ending a chat entry while a match plays in the window leaves the
// engine's token guard alone, which would also drop the watched match's own
// input reads on the next frame.
func TestLobbyEditTextTokenGuardWhileWatching(t *testing.T) {
	runLobbyLua(t, `
		keyDown = 1
		watchActive = true
		check(I.editText({text = 'hi'}, 60) == true, 'confirm')
		escDown = true
		check(I.editText({text = 'hi'}, 60) == false, 'cancel')
		check(count('resetTokenGuard') == 0, 'token guard reset while watching')
		watchActive = false
		escDown = false
		check(I.editText({text = 'hi'}, 60) == true, 'confirm')
		escDown = true
		check(I.editText({text = 'hi'}, 60) == false, 'cancel')
		check(count('resetTokenGuard') == 2, 'token guard not reset outside a watch')
	`)
}

// The match clock, the results countdown and the ping interval follow the
// wall clock, not the room's ticks (4 per frame while a watch catches up).
func TestLobbyClocksUseWallTime(t *testing.T) {
	runLobbyLua(t, `
		nakama.ping = function() table.insert(calls, {name = 'ping', args = {}}) end
		local s = playing(1, true)
		s.phase_remaining = 10
		s.current.elapsed = 5
		setState(s)
		for i = 1, 240 do
			lobby.f_poll()
			lobby.f_tickPing()
		end
		check(I.matchElapsed() == 5, 'elapsed moved with ticks: ' .. I.matchElapsed())
		check(I.phaseRemaining() == 10, 'countdown moved with ticks: ' .. I.phaseRemaining())
		check(count('ping') == 1, 'pings while the clock stood still: ' .. count('ping'))
		now = now + 3
		lobby.f_poll()
		lobby.f_tickPing()
		check(I.matchElapsed() == 8, 'elapsed after 3 s: ' .. I.matchElapsed())
		check(I.phaseRemaining() == 7, 'countdown after 3 s: ' .. I.phaseRemaining())
		check(count('ping') == 2, 'no ping after the interval')
	`)
}

// The direct link to the host starts as soon as the window waits for a
// started stream, once per pairing, and closes when the wait is given up.
func TestLobbyWatchLinkStartsWhileWaiting(t *testing.T) {
	runLobbyLua(t, `
		local rs = newRoom()
		setState(playing(4, false))
		lobby.f_poll()
		I.updateWatch(rs)
		check(rs.watch.seq == 4, 'not waiting for the match')
		check(count('startWatchLink') == 0, 'link before the stream started')
		setState(playing(4, true))
		lobby.f_poll()
		I.updateWatch(rs)
		I.updateWatch(rs)
		check(count('startWatchLink') == 1, 'link started ' .. count('startWatchLink') .. ' times')
		check(calls[lastIndex('startWatchLink')].args[1] == 'host', 'link to the wrong member')
		-- The next pairing: the old link closes and a new one opens.
		setState(playing(5, true))
		lobby.f_poll()
		I.updateWatch(rs)
		check(count('stopWatchLink') == 1 and count('startWatchLink') == 2, 'no new link for the next pairing')
		-- WATCH OFF (or a skipped match): the wait ends and so does the link.
		rs.watch.done[5] = true
		I.updateWatch(rs)
		I.updateWatch(rs)
		check(rs.watch.seq == nil, 'still waiting')
		check(count('stopWatchLink') == 2, 'link left open: ' .. count('stopWatchLink'))
		-- This player's own match.
		setState(playing(6, true))
		lobby.f_poll()
		I.updateWatch(rs)
		check(count('startWatchLink') == 3, 'no link for pairing 6')
		local s = playing(7, true)
		s.current.player2 = 'me'
		setState(s)
		lobby.f_poll()
		I.updateWatch(rs)
		check(count('stopWatchLink') == 3, 'link left open for my own match')
		check(count('startWatchLink') == 3, 'linked to my own match')
	`)
}

// A pairing that is not ready after all is retried, not skipped for good.
func TestLobbyWatchInRoomNotReadyRetries(t *testing.T) {
	runLobbyLua(t, `
		local rs = newRoom()
		setState(playing(4, true))
		lobby.f_poll()
		stream = readyStream(4)
		stream.ready = false
		rs.watch.seq = 4
		I.watchInRoom(rs, 4)
		check(rs.watch.done[4] == nil, 'pairing marked done')
		check(rs.watch.seq == nil, 'still waiting')
		check(count('startSpectatorReplay') == 0, 'replay started')
	`)
}

// A failure between the replay's start and the fight undoes the watch.
func TestLobbyWatchInRoomSetupFailureCleansUp(t *testing.T) {
	runLobbyLua(t, `
		local rs = newRoom()
		setState(playing(4, true))
		lobby.f_poll()
		stream = readyStream(4)
		local game = start.f_game
		lobby.f_setupMatch = function() error('boom') end
		launchFight = rec('launchFight')
		I.watchInRoom(rs, 4)
		check(count('launchFight') == 0, 'fight launched')
		check(rs.watch.done[4] == true, 'pairing not marked done')
		check(rs.watching == false and rs.watch.seq == nil, 'still watching')
		check(count('clearWatchWindow') == 1, 'window left set')
		check(count('hook.stop') == 3, 'hooks left')
		check(count('startWatchLink') == 1 and count('stopWatchLink') == 1, 'link left open')
		check(count('exitReplay') == 1, 'replay left running')
		local last = calls[lastIndex('lobbySend')].args
		check(last[1] == 'watching' and last[2].watching == false, 'server still told watching')
		check(start.f_game == game, 'start.f_game left wrapped')
		check(gameOption('Config.VsScreenLoading') == true, 'VS screen loading left off')
		check(L.chat[#L.chat] ~= nil and L.chat[#L.chat].kind == 'error', 'no error in the chat')
	`)
}

// The watch loads through the engine (VS screen loading reads as off for
// the fight) without changing the option itself.
func TestLobbyWatchInRoomEngineLoading(t *testing.T) {
	runLobbyLua(t, `
		local rs = newRoom()
		setState(playing(4, true))
		lobby.f_poll()
		stream = readyStream(4)
		lobby.f_setupMatch = function() end
		local seen = nil
		launchFight = function(t)
			table.insert(calls, {name = 'launchFight', args = {t}})
			seen = gameOption('Config.VsScreenLoading')
			check(rs.watching == true, 'not watching during the fight')
			return true
		end
		I.watchInRoom(rs, 4)
		check(count('launchFight') == 1, 'no fight')
		check(seen == false, 'VS screen loading read as on during the fight')
		check(count('modifyGameOption') == 0, 'game option changed')
		check(gameOption('Config.VsScreenLoading') == true, 'VS screen loading left off')
		check(gameOption('Config.Other') == 0, 'other options changed')
		check(rs.watching == false and count('clearWatchWindow') == 1, 'not cleaned up')
		check(count('stopWatchLink') == 1, 'link left open')
	`)
}
