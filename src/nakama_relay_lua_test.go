package main

// The server's relay (nakama/modules/ikemen_relay.lua), and the session and
// lobby modules that make it, run with gopher-lua (of which the Lua of
// Nakama's runtime is a fork) and a stand-in for Nakama's runtime module: who
// the relay admits, what it passes on and to whom, its limits, when it ends,
// and which pairs the ikemen_relay RPC gets a relay for. The live tests
// (nakama_relay_live_test.go) run the modules in Nakama.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// luaToGo converts a Lua value to what encoding/json writes, as Nakama's
// json_encode does: a table with only list items is an array, any other an
// object.
func luaToGo(v lua.LValue) any {
	switch v := v.(type) {
	case lua.LBool:
		return bool(v)
	case lua.LNumber:
		return float64(v)
	case lua.LString:
		return string(v)
	case *lua.LTable:
		if n := v.MaxN(); n > 0 {
			list := make([]any, 0, n)
			for i := 1; i <= n; i++ {
				list = append(list, luaToGo(v.RawGetInt(i)))
			}
			return list
		}
		obj := map[string]any{}
		v.ForEach(func(k, e lua.LValue) { obj[k.String()] = luaToGo(e) })
		return obj
	}
	return nil
}

// relayLua runs the server modules. Matches the modules create are "running"
// until a test ends them (ended), and match_signal calls the function a test
// put in signal_targets[match id].
type relayLua struct {
	l    *lua.LState
	logs []string
}

const relayLuaPrelude = `
relay = require("ikemen_relay")
OP_RELAY = 0x494B524C
signal_targets = {}
created = {}
running = {}

function presence(user, session)
    return { user_id = user, session_id = session or ("session-" .. user), username = user, node = "node" }
end

function new_dispatcher()
    local d = { sent = {} }
    d.broadcast_message = function(op, data, to, sender, reliable)
        d.sent[#d.sent + 1] = { op = op, data = data, to = to, sender = sender, reliable = reliable }
    end
    return d
end

function message(p, data, op)
    return { sender = p, op_code = op or OP_RELAY, data = data, reliable = false }
end

function check(cond, ...)
    if not cond then
        error(string.format(...), 2)
    end
end

-- The match id of an ikemen_relay reply.
function relay_reply_id(reply)
    check(type(reply) == "string", "no reply")
    local id = reply:match('^{"match_id":"([^"]+)"}$')
    check(id ~= nil, "reply %s", reply)
    return id
end

-- Calls the ikemen_relay RPC as user_id: its reply, or its error's message and
-- code.
function call_relay(user_id, payload)
    local ok, result = pcall(rpcs.ikemen_relay, { user_id = user_id, env = {} }, payload)
    if ok then
        return result, nil, nil
    end
    if type(result) == "table" then
        return nil, result[1], result[2]
    end
    return nil, tostring(result), nil
end
`

func loadRelayLua(t *testing.T) *relayLua {
	t.Helper()
	m := &relayLua{l: lua.NewState()}
	t.Cleanup(m.l.Close)
	m.l.PreloadModule("nakama", func(l *lua.LState) int {
		nk := l.NewTable()
		set := func(name string, fn lua.LGFunction) { nk.RawSetString(name, l.NewFunction(fn)) }
		set("json_decode", func(l *lua.LState) int {
			var v any
			if err := json.Unmarshal([]byte(l.CheckString(1)), &v); err != nil {
				l.RaiseError("json_decode: %v", err)
			}
			l.Push(jsonToLua(l, v))
			return 1
		})
		set("json_encode", func(l *lua.LState) int {
			b, err := json.Marshal(luaToGo(l.Get(1)))
			if err != nil {
				l.RaiseError("json_encode: %v", err)
			}
			l.Push(lua.LString(b))
			return 1
		})
		for _, name := range []string{"logger_debug", "logger_info", "logger_warn", "logger_error"} {
			name := name
			set(name, func(l *lua.LState) int {
				m.logs = append(m.logs, name+": "+l.CheckString(1))
				return 0
			})
		}
		set("time", func(l *lua.LState) int {
			l.Push(lua.LNumber(time.Now().UnixMilli()))
			return 1
		})
		set("register_rpc", func(l *lua.LState) int {
			fn, id := l.CheckFunction(1), l.CheckString(2)
			rpcs, ok := l.GetGlobal("rpcs").(*lua.LTable)
			if !ok {
				rpcs = l.NewTable()
				l.SetGlobal("rpcs", rpcs)
			}
			rpcs.RawSetString(id, fn)
			return 0
		})
		// match_create records the module and its params in created, and the
		// match runs (running[id]) until a test ends it.
		set("match_create", func(l *lua.LState) int {
			module, params := l.CheckString(1), l.OptTable(2, l.NewTable())
			created := l.GetGlobal("created").(*lua.LTable)
			id := fmt.Sprintf("%08d-0000-4000-8000-000000000000.node", created.Len()+1)
			entry := l.NewTable()
			entry.RawSetString("id", lua.LString(id))
			entry.RawSetString("module", lua.LString(module))
			entry.RawSetString("params", params)
			created.Append(entry)
			l.GetGlobal("running").(*lua.LTable).RawSetString(id, lua.LTrue)
			l.Push(lua.LString(id))
			return 1
		})
		set("match_get", func(l *lua.LState) int {
			id := l.CheckString(1)
			if l.GetGlobal("running").(*lua.LTable).RawGetString(id) != lua.LTrue {
				l.Push(lua.LNil)
				return 1
			}
			match := l.NewTable()
			match.RawSetString("match_id", lua.LString(id))
			l.Push(match)
			return 1
		})
		set("match_signal", func(l *lua.LState) int {
			id, data := l.CheckString(1), l.OptString(2, "")
			fn, ok := l.GetGlobal("signal_targets").(*lua.LTable).RawGetString(id).(*lua.LFunction)
			if !ok {
				l.RaiseError("match not found")
			}
			if err := l.CallByParam(lua.P{Fn: fn, NRet: 1, Protect: true}, lua.LString(data)); err != nil {
				l.RaiseError("%v", err)
			}
			ret := l.Get(-1)
			l.Pop(1)
			l.Push(ret)
			return 1
		})
		l.Push(nk)
		return 1
	})
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "nakama", "modules")
	m.run(t, fmt.Sprintf("package.path = %q", filepath.ToSlash(filepath.Join(dir, "?.lua"))))
	m.run(t, relayLuaPrelude)
	return m
}

func (m *relayLua) run(t *testing.T, script string) {
	t.Helper()
	if err := m.l.DoString(script); err != nil {
		t.Fatal(err)
	}
}

func (m *relayLua) logged(level, text string) int {
	count := 0
	for _, line := range m.logs {
		if strings.HasPrefix(line, level+": ") && strings.Contains(line, text) {
			count++
		}
	}
	return count
}

// A relay for a and b, both joined: relay_state, dispatcher, pa and pb.
const relayLuaPair = `
ctx = { match_id = "relay.node", env = {} }
local rate, label
relay_state, rate, label = relay.match_init(ctx, { player1 = "a", player2 = "b", parent = "session.node" })
check(rate == 60, "tick rate %s", tostring(rate))
check(label == '{"kind":"ikemen-relay"}', "label %s", tostring(label))
dispatcher = new_dispatcher()
pa, pb = presence("a"), presence("b")
local ok
relay_state, ok = relay.match_join_attempt(ctx, dispatcher, 0, relay_state, pa, {})
check(ok, "a was refused")
relay_state = relay.match_join(ctx, dispatcher, 0, relay_state, { pa })
relay_state, ok = relay.match_join_attempt(ctx, dispatcher, 0, relay_state, pb, {})
check(ok, "b was refused")
relay_state = relay.match_join(ctx, dispatcher, 0, relay_state, { pb })
`

// The relay admits its two players only, each with one presence.
func TestRelayLuaAdmitsThePairOnly(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, `
local ctx = { match_id = "relay.node", env = {} }
local state = relay.match_init(ctx, { player1 = "a", player2 = "b", parent = "session.node" })
local d = new_dispatcher()
local ok, reason
state, ok, reason = relay.match_join_attempt(ctx, d, 0, state, presence("stranger"), {})
check(not ok and reason ~= nil, "a stranger was admitted")
state, ok = relay.match_join_attempt(ctx, d, 0, state, presence("a"), {})
check(ok, "a was refused")
state = relay.match_join(ctx, d, 0, state, { presence("a") })
state, ok = relay.match_join_attempt(ctx, d, 0, state, presence("a", "another-session"), {})
check(not ok, "a second presence of a was admitted")
state, ok = relay.match_join_attempt(ctx, d, 0, state, presence("b"), {})
check(ok, "b was refused")
for _, params in ipairs({ {}, { player1 = "a" }, { player1 = "a", player2 = "a" } }) do
    check(not pcall(relay.match_init, ctx, params), "a relay was made for %s and %s", tostring(params.player1), tostring(params.player2))
end
`)
}

// Each player's relay data goes to the other player only, with its sender;
// other messages, and data over the size limit, go nowhere.
func TestRelayLuaPassesDataToTheOtherPlayerOnly(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, relayLuaPair+`
relay_state = relay.match_loop(ctx, dispatcher, 1, relay_state, {
    message(pa, "from a"),
    message(pb, "from b"),
    message(pa, "a's other data", 0x494B5244),
    message(pa, ""),
    message(pa, string.rep("x", 2049)),
    message(pb, string.rep("y", 2048)),
})
local sent = dispatcher.sent
check(#sent == 3, "passed on %d datagrams, want 3", #sent)
local function went(i, data, to, from)
    local s = sent[i]
    check(s.op == OP_RELAY and s.data == data and #s.to == 1 and s.to[1].user_id == to and s.to[1].session_id == "session-" .. to
        and s.sender.user_id == from and s.reliable == false, "datagram %d: %s from %s to %s", i, tostring(s.data):sub(1, 20),
        tostring(s.sender and s.sender.user_id), tostring(s.to[1] and s.to[1].user_id))
end
went(1, "from a", "b", "a")
went(2, "from b", "a", "b")
went(3, string.rep("y", 2048), "a", "b")
`)
	if m.logged("logger_warn", "a sends more than the relay passes on") != 1 {
		t.Fatalf("logs %q; want one warning about a", m.logs)
	}
}

// What each player sends is limited: 16 datagrams and 32 KB at once, then 5
// datagrams and about 1 KB each tick (300 and 64 KB a second); each player
// has a limit of its own, and the log says once that a player went over it.
func TestRelayLuaLimitsEachPlayersRate(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, relayLuaPair+`
local function burst(p, count, size)
    local messages = {}
    for i = 1, count do
        messages[i] = message(p, string.rep("d", size))
    end
    return messages
end
local function passed(tick, messages)
    local before = #dispatcher.sent
    relay_state = relay.match_loop(ctx, dispatcher, tick, relay_state, messages)
    return #dispatcher.sent - before
end
check(passed(1, burst(pa, 40, 10)) == 16, "a's burst")
check(passed(2, burst(pa, 40, 10)) == 5, "a's next tick")
check(passed(3, burst(pb, 40, 10)) == 16, "b's burst, after a's")
-- Bytes: 16 datagrams of 2 KB empty b's 32 KB; a tick then brings 1092
-- bytes, short of a 1500-byte datagram, and the next one enough.
for tick = 4, 100 do
    relay_state = relay.match_loop(ctx, dispatcher, tick, relay_state, {})
end
check(passed(101, burst(pb, 17, 2048)) == 16, "b's 2 KB burst")
check(passed(102, burst(pb, 1, 1500)) == 0, "1500 bytes one tick later")
check(passed(103, burst(pb, 1, 1500)) == 1, "1500 bytes two ticks later")
-- Over a second, at most 300 datagrams.
local total = 0
for tick = 200, 259 do
    total = total + passed(tick, burst(pa, 10, 10))
end
check(total >= 300 and total <= 316, "a passed %d datagrams in a second", total)
`)
	if m.logged("logger_warn", "a sends more") != 1 || m.logged("logger_warn", "b sends more") != 1 {
		t.Fatalf("logs %q; want one warning about each player", m.logs)
	}
}

// The relay ends when one of its players leaves, and after 30 seconds without
// data from either.
func TestRelayLuaEnds(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, relayLuaPair+`
local state = relay.match_leave(ctx, dispatcher, 5, relay_state, { presence("b", "an-older-session") })
check(state ~= nil, "the leave of a session the relay does not hold ended it")
check(relay.match_leave(ctx, dispatcher, 5, relay_state, { pb }) == nil, "b left, and the relay goes on")

relay_state = relay.match_init(ctx, { player1 = "a", player2 = "b" })
for tick = 1, 1800 do
    relay_state = relay.match_loop(ctx, dispatcher, tick, relay_state, {})
    check(relay_state ~= nil, "ended at tick %d, before 30 seconds", tick)
end
check(relay.match_loop(ctx, dispatcher, 1801, relay_state, {}) == nil, "nobody joined or sent anything for 30 seconds, and the relay goes on")

relay_state = relay.match_init(ctx, { player1 = "a", player2 = "b" })
relay_state = relay.match_join(ctx, dispatcher, 0, relay_state, { pa })
relay_state = relay.match_loop(ctx, dispatcher, 1000, relay_state, { message(pa, "hello") })
check(relay.match_loop(ctx, dispatcher, 2800, relay_state, {}) ~= nil, "ended 30 seconds after data")
check(relay.match_loop(ctx, dispatcher, 2801, relay_state, {}) == nil, "no data for over 30 seconds, and the relay goes on")
check(select(2, relay.match_signal(ctx, dispatcher, 1, relay_state, "{}")) == nil, "a relay answered a signal")
`)
}

// The ikemen_relay RPC: a session or lobby answers with the relay of a pair it
// made, the one it made before while that runs, and refuses anyone else.
func TestRelayLuaRPC(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, `
-- A parent that pairs a and b.
local parent = { match_id = "parent.node" }
local parent_state = {}
signal_targets["parent.node"] = function(data)
    return relay.answer(parent, parent_state, data, function(x, y)
        return (x == "a" and y == "b") or (x == "b" and y == "a")
    end)
end
signal_targets["echo.node"] = function(data) return data end
signal_targets["failing.node"] = function(data) return '{"error":"failed"}' end

local reply, err, code = call_relay("a", '{"match_id":"parent.node","peer_id":"b"}')
check(reply ~= nil, "a's call failed: %s", tostring(err))
local first = relay_reply_id(reply)
check(#created == 1 and created[1].module == "ikemen_relay" and created[1].id == first, "relays made: %d", #created)
local params = created[1].params
check(params.player1 == "a" and params.player2 == "b" and params.parent == "parent.node", "relay params %s %s %s",
    tostring(params.player1), tostring(params.player2), tostring(params.parent))
reply = call_relay("b", '{"match_id":"parent.node","peer_id":"a"}')
check(relay_reply_id(reply) == first and #created == 1, "b got another relay than a")

running[first] = nil
reply = call_relay("b", '{"match_id":"parent.node","peer_id":"a"}')
local second = relay_reply_id(reply)
check(second ~= first and #created == 2, "the relay ended, and the pair still got it")

for _, case in ipairs({
    { user = nil, payload = '{"match_id":"parent.node","peer_id":"b"}', code = 16 },
    { user = "", payload = '{"match_id":"parent.node","peer_id":"b"}', code = 16 },
    { user = "a", payload = 'not json', code = 3 },
    { user = "a", payload = '{"match_id":"parent.node"}', code = 3 },
    { user = "a", payload = '{"match_id":"","peer_id":"b"}', code = 3 },
    { user = "a", payload = '{"match_id":"parent.node","peer_id":7}', code = 3 },
    { user = "a", payload = '{"match_id":"parent.node","peer_id":"' .. string.rep("b", 129) .. '"}', code = 3 },
    { user = "c", payload = '{"match_id":"parent.node","peer_id":"a"}', code = 9 },
    { user = "a", payload = '{"match_id":"parent.node","peer_id":"c"}', code = 9 },
    { user = "a", payload = '{"match_id":"parent.node","peer_id":"a"}', code = 9 },
    { user = "a", payload = '{"match_id":"nowhere.node","peer_id":"b"}', code = 9 },
    { user = "a", payload = '{"match_id":"echo.node","peer_id":"b"}', code = 9 },
    { user = "a", payload = '{"match_id":"failing.node","peer_id":"b"}', code = 13 },
}) do
    local reply, err, code = call_relay(case.user, case.payload)
    check(reply == nil and code == case.code, "%s as %s: %s, error %s (%s), want code %d", case.payload, tostring(case.user),
        tostring(reply), tostring(err), tostring(code), case.code)
end
check(#created == 2, "refused calls made relays")
check(relay.answer(parent, parent_state, '{"kind":"ranked_claim","set":1}', function() return true end) == nil,
    "another signal was taken for the relay's")
`)
}

// A session gets the relay for two of its players, matched into it; a lobby
// for the pairing playing now.
func TestRelayLuaSessionAndLobbyPairs(t *testing.T) {
	m := loadRelayLua(t)
	m.run(t, `
local session = require("ikemen_session")
local sctx = { match_id = "session.node", env = {} }
local expected = {}
for _, user in ipairs({ "a", "b" }) do
    expected[#expected + 1] = { presence = presence(user) }
end
local sstate = session.match_init(sctx, { mode = "unranked", expected_users = expected })
local d = new_dispatcher()
sstate = session.match_join(sctx, d, 1, sstate, { presence("a") })
signal_targets["session.node"] = function(data)
    local state, answer = session.match_signal(sctx, d, 1, sstate, data)
    sstate = state
    return answer
end
local reply, err = call_relay("a", '{"match_id":"session.node","peer_id":"b"}')
check(reply == nil, "a relay for a and b before b joined the session")
sstate = session.match_join(sctx, d, 1, sstate, { presence("b") })
reply, err = call_relay("a", '{"match_id":"session.node","peer_id":"b"}')
check(reply ~= nil, "a's call in the session failed: %s", tostring(err))
check(relay_reply_id(call_relay("b", '{"match_id":"session.node","peer_id":"a"}')) == relay_reply_id(reply), "a and b got two relays")
-- c joins the session (as a session without an expected list would let it).
sstate.players["c"] = { presence = presence("c") }
check(call_relay("c", '{"match_id":"session.node","peer_id":"a"}') == nil, "a relay for c, whom the matchmaker did not match")
check(select(2, session.match_signal(sctx, d, 1, sstate, '{"kind":"ranked_claim","set":9}')) == '{"kind":"ranked_claim","set":9}',
    "the session's other signals changed")

local lobby = require("ikemen_lobby")
local lctx = { match_id = "lobby.node", env = {} }
local lstate = lobby.match_init(lctx, { creator = "a", game = "", build = "" })
for _, user in ipairs({ "a", "b", "c" }) do
    lstate.players[user] = { session_id = "session-" .. user, presence = presence(user) }
end
signal_targets["lobby.node"] = function(data)
    local state, answer = lobby.match_signal(lctx, d, 1, lstate, data)
    lstate = state
    return answer
end
check(call_relay("a", '{"match_id":"lobby.node","peer_id":"b"}') == nil, "a relay with no pairing playing")
lstate.current_match = { player1 = "a", player2 = "b" }
reply, err = call_relay("b", '{"match_id":"lobby.node","peer_id":"a"}')
check(reply ~= nil, "b's call in the lobby failed: %s", tostring(err))
check(call_relay("c", '{"match_id":"lobby.node","peer_id":"a"}') == nil, "a relay for a member who is not playing")
check(call_relay("a", '{"match_id":"lobby.node","peer_id":"c"}') == nil, "a relay for a and a member who is not playing")
lstate.players["b"] = nil
check(call_relay("a", '{"match_id":"lobby.node","peer_id":"b"}') == nil, "a relay with a player who left the lobby")
check(select(2, lobby.match_signal(lctx, d, 1, lstate, "other")) == "other", "the lobby's other signals changed")
`)
}
