package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	lua "github.com/yuin/gopher-lua"
)

func gameOptionInt(path string) int {
	value, err := sys.cfg.GetValue(path)
	if err != nil {
		return 0
	}
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func gameOptionBool(path string) bool {
	value, err := sys.cfg.GetValue(path)
	if err != nil {
		return false
	}
	b, ok := value.(bool)
	return ok && b
}

// nakamaLuaHandlers holds the Lua event handlers: one per event set with
// nakama.on(event, fn), and any number set with nakama.on(event, fn, name).
// A script that names its handler (the lobby screens do) keeps it when
// another script sets the unnamed handler of the same event.
type nakamaLuaHandlers struct {
	unnamed map[string]*lua.LFunction
	named   map[string]map[string]*lua.LFunction
}

func newNakamaLuaHandlers() *nakamaLuaHandlers {
	return &nakamaLuaHandlers{
		unnamed: make(map[string]*lua.LFunction),
		named:   make(map[string]map[string]*lua.LFunction),
	}
}

// set installs fn for event (name "" is the unnamed handler); a nil fn
// removes a named handler.
func (h *nakamaLuaHandlers) set(event, name string, fn *lua.LFunction) {
	if name == "" {
		h.unnamed[event] = fn
		return
	}
	if fn == nil {
		delete(h.named[event], name)
		return
	}
	if h.named[event] == nil {
		h.named[event] = make(map[string]*lua.LFunction)
	}
	h.named[event][name] = fn
}

// forEvent returns the handlers of event: the unnamed one first, then the
// named ones in name order.
func (h *nakamaLuaHandlers) forEvent(event string) []*lua.LFunction {
	var out []*lua.LFunction
	if fn := h.unnamed[event]; fn != nil {
		out = append(out, fn)
	}
	names := make([]string, 0, len(h.named[event]))
	for name := range h.named[event] {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, h.named[event][name])
	}
	return out
}

func nakamaScriptInit(l *lua.LState) {
	api := l.NewTable()
	callbacks := newNakamaLuaHandlers()

	api.RawSetString("connect", l.NewFunction(func(l *lua.LState) int {
		cfg := NakamaConfig{
			Host:       "127.0.0.1",
			Port:       7350,
			ServerKey:  "defaultkey",
			UseTLS:     false,
			Status:     true,
			DeviceID:   "",
			Username:   "",
			Game:       "",
			GameBuild:  "",
			Region:     "",
			RequestTTL: defaultNakamaRequestTTL,
		}
		stunServers := []string(nil)

		// Start from the shipped server profile so a game can simply call
		// nakama.connect() or provide a partial table of overrides. Developers can
		// select another profile with -nakama-server or server_id = "...".
		if loaded, stun, err := defaultNakamaClientConfig(); err == nil {
			cfg = loaded
			stunServers = stun
		} else if sys.cmdFlags != nil {
			log.Printf("Nakama server profile unavailable; using local development fallback: %v", err)
		}

		if l.GetTop() >= 1 && l.Get(1) != lua.LNil {
			t := tableArg(l, 1)
			if serverID := luaStringField(t, "server_id", ""); serverID != "" {
				path := defaultNakamaProfilePath
				if val, ok := sys.cmdFlags["-nakama-config"]; ok && strings.TrimSpace(val) != "" {
					path = val
				}
				if profiles, err := LoadNakamaServerProfiles(path); err == nil {
					if profile, ok := profiles.Find(serverID); ok {
						cfg = profile.clientConfig()
						stunServers = append([]string(nil), profile.STUNServers...)
					}
				}
			}
			cfg.Host = luaStringField(t, "host", cfg.Host)
			cfg.Port = luaIntField(t, "port", cfg.Port)
			cfg.ServerKey = luaStringField(t, "server_key", cfg.ServerKey)
			cfg.UseTLS = luaBoolField(t, "ssl", cfg.UseTLS)
			cfg.Status = luaBoolField(t, "status", cfg.Status)
			cfg.DeviceID = luaStringField(t, "device_id", cfg.DeviceID)
			cfg.Username = luaStringField(t, "username", cfg.Username)
			cfg.Game = luaStringField(t, "game", cfg.Game)
			cfg.GameBuild = luaStringField(t, "build", cfg.GameBuild)
			cfg.Region = luaStringField(t, "region", cfg.Region)
			if timeout := luaIntField(t, "timeout_ms", int(cfg.RequestTTL/time.Millisecond)); timeout > 0 {
				cfg.RequestTTL = time.Duration(timeout) * time.Millisecond
			}
		}

		if sys.nakama != nil {
			sys.nakama.Disconnect()
		}
		n := NewNakamaClient(cfg)
		n.SetSTUNServers(stunServers)
		n.SetEventHandler(func(ev NakamaEvent) {
			// Realtime events are parsed off-thread. gopher-lua stays on IKEMEN's main thread.
			// Events of a client that nakama.connect has since replaced are dropped
			// there; a disconnected client's events (a failed reconnect, the result
			// of a name change) still reach Lua.
			task := func() {
				if sys.nakama != n {
					return
				}
				nakamaDispatchLua(l, callbacks, ev)
			}
			select {
			case sys.mainThreadTask <- task:
			default:
				log.Printf("Nakama main-thread event queue is full; dropping %q", ev.Type)
			}
		})
		sys.nakama = n
		SafeGo(func() {
			if err := n.Connect(context.Background()); err != nil {
				n.emit(NakamaEvent{Type: "error", Error: err})
				return
			}
			n.emit(NakamaEvent{Type: "connected"})
		})
		return 0
	}))

	api.RawSetString("disconnect", l.NewFunction(func(*lua.LState) int {
		if sys.nakama != nil {
			sys.nakama.Disconnect()
		}
		return 0
	}))

	// on(event, fn[, name]): fn handles event. Without a name it replaces the
	// event's handler; with a name it is kept beside the others, and
	// on(event, nil, name) removes it.
	api.RawSetString("on", l.NewFunction(func(l *lua.LState) int {
		event := strArg(l, 1)
		name := optionalLuaString(l, 3, "")
		fn, ok := l.Get(2).(*lua.LFunction)
		if !ok && !(name != "" && l.Get(2) == lua.LNil) {
			l.RaiseError("nakama.on requires a function")
		}
		callbacks.set(event, name, fn)
		return 0
	}))

	api.RawSetString("matchmake", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		mode := optionalLuaString(l, 1, "unranked")
		bestOf := gameOptionInt("Netplay.Ranked.BestOf")
		switchSides := gameOptionBool("Netplay.Ranked.SwitchSides")
		winnerKeeps := gameOptionBool("Netplay.Ranked.WinnerKeepsSelection")
		req := NakamaMatchmakerRequest{
			Mode:                       mode,
			Elo:                        optionalLuaInt(l, 2, 1000),
			EloRange:                   optionalLuaInt(l, 3, 100),
			Game:                       optionalLuaString(l, 4, ""),
			GameBuild:                  optionalLuaString(l, 5, ""),
			Region:                     optionalLuaString(l, 6, ""),
			MinCount:                   2,
			MaxCount:                   2,
			RankedBestOf:               bestOf,
			RankedSwitchSides:          switchSides,
			RankedWinnerKeepsSelection: winnerKeeps,
		}
		if err := sys.nakama.Matchmake(req); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("cancelMatchmaking", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama != nil {
			if err := sys.nakama.CancelMatchmaking(); err != nil {
				l.RaiseError(err.Error())
			}
		}
		return 0
	}))

	api.RawSetString("joinMatch", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		matchID := optionalLuaString(l, 1, "")
		token := optionalLuaString(l, 2, "")
		metadata := map[string]string{}
		if l.GetTop() >= 3 && l.Get(3) != lua.LNil {
			t := tableArg(l, 3)
			// code is the room ID a private lobby asks for.
			for _, key := range []string{"game", "build", "code"} {
				value := luaStringField(t, key, "")
				if value != "" {
					metadata[key] = value
				}
			}
		}
		if err := sys.nakama.JoinMatchWithMetadata(matchID, token, metadata); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("leaveMatch", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama != nil {
			if err := sys.nakama.LeaveMatch(); err != nil {
				l.RaiseError(err.Error())
			}
		}
		return 0
	}))

	api.RawSetString("startP2P", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		port := optionalLuaInt(l, 1, sys.cfg.Netplay.Rollback.Port)
		stun := optionalLuaString(l, 2, "")
		var servers []string
		if stun != "" {
			for _, entry := range strings.Split(stun, ",") {
				entry = strings.TrimSpace(entry)
				if entry != "" {
					servers = append(servers, entry)
				}
			}
		}
		if err := sys.nakama.StartP2P(port, servers); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("stopP2P", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama != nil {
			sys.nakama.StopP2P()
		}
		return 0
	}))

	// status returns a snapshot for UI polling: connected, matchmaking,
	// match_id, p2p (none, connecting, ready, failed, closed), host and peer
	// when the pairing is known, and the last reported error.
	api.RawSetString("status", l.NewFunction(func(l *lua.LState) int {
		t := l.NewTable()
		if sys.nakama == nil {
			t.RawSetString("connected", lua.LFalse)
			t.RawSetString("matchmaking", lua.LFalse)
			t.RawSetString("match_id", lua.LString(""))
			t.RawSetString("p2p", lua.LString("none"))
			l.Push(t)
			return 1
		}
		st := sys.nakama.Status()
		t.RawSetString("connected", lua.LBool(st.Connected))
		t.RawSetString("matchmaking", lua.LBool(st.Matchmaking))
		t.RawSetString("match_id", lua.LString(st.MatchID))
		t.RawSetString("p2p", lua.LString(st.P2P))
		if st.Pairing != nil {
			t.RawSetString("host", lua.LBool(st.Pairing.Host))
			t.RawSetString("peer", lua.LString(st.Pairing.PeerUserID))
		}
		if st.LastError != "" {
			t.RawSetString("error", lua.LString(st.LastError))
		}
		// rtt_ms: last measured server round trip (nakama.ping), -1 until known.
		rtt := -1
		if st.RTT > 0 {
			rtt = int(st.RTT / time.Millisecond)
		}
		t.RawSetString("rtt_ms", lua.LNumber(rtt))
		t.RawSetString("region", lua.LString(st.Region))
		t.RawSetString("game", lua.LString(st.Game))
		t.RawSetString("build", lua.LString(st.GameBuild))
		// connection: wired, wifi, mobile or "" (unknown).
		t.RawSetString("connection", lua.LString(st.Connection))
		// p2p_rtt_ms: round trip to the paired player over the P2P path, -1
		// until measured.
		p2pRTT := -1
		if st.P2PRTT > 0 {
			p2pRTT = int(st.P2PRTT / time.Millisecond)
		}
		t.RawSetString("p2p_rtt_ms", lua.LNumber(p2pRTT))
		// netplay: none, connecting, connected or failed (session handshake
		// over the P2P stream; see nakama.enterNetPlay).
		netplay := "none"
		if nc := sys.netConnection; nc != nil {
			switch {
			case nc.hasConn():
				netplay = "connected"
			case nc.AttachError() != nil:
				netplay = "failed"
				t.RawSetString("netplay_error", lua.LString(nc.AttachError().Error()))
			default:
				netplay = "connecting"
			}
		}
		t.RawSetString("netplay", lua.LString(netplay))
		l.Push(t)
		return 1
	}))

	api.RawSetString("clearError", l.NewFunction(func(*lua.LState) int {
		if sys.nakama != nil {
			sys.nakama.ClearLastError()
		}
		return 0
	}))

	// setPairing(peerUserId, host) records a pairing made outside the
	// matchmaker (a lobby pairing), before startP2P and enterNetPlay.
	api.RawSetString("setPairing", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		sys.nakama.SetPairing(sys.nakama.CurrentMatchID(), strArg(l, 1), boolArg(l, 2))
		return 0
	}))

	// enterNetPlay([host]) is enterNetPlay() for a Nakama pairing: the session
	// stream runs over the hole-punched P2P socket instead of a TCP connection
	// to an IP address. host defaults to the pairing's role. connected() turns
	// true once both sides finish the session handshake.
	api.RawSetString("enterNetPlay", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		if sys.netConnection != nil {
			l.RaiseError("\nConnection already established.\n")
		}
		var host bool
		if l.GetTop() >= 1 && l.Get(1) != lua.LNil {
			host = boolArg(l, 1)
		} else if pairing, ok := sys.nakama.Pairing(); ok {
			host = pairing.Host
		} else {
			l.RaiseError("nakama.enterNetPlay: the host role is unknown; pass true or false")
		}
		stream, err := sys.nakama.OpenP2PStream()
		if err != nil {
			l.RaiseError("%s", err.Error())
		}
		// Mirrors enterNetPlay in script.go.
		sys.sessionWarning = ""
		sys.chars = [len(sys.chars)][]*Char{}
		sys.netConnection = NewNetConnection()
		if sys.cfg.Netplay.RollbackNetcode {
			rs := NewRollbackSession(sys.cfg.Netplay.Rollback)
			sys.rollback.session = &rs
			if !host {
				// A non-empty host marks the guest side (see preMatchSetup).
				sys.rollback.session.host = nakamaP2PHostLabel
			}
		}
		sys.netConnection.AttachStream(stream, host)
		return 0
	}))

	api.RawSetString("p2pReady", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.Push(lua.LFalse)
			return 1
		}
		l.Push(lua.LBool(sys.nakama.P2PReady()))
		return 1
	}))

	// replayStatus() describes the replay stream that startSpectatorReplay
	// would play: the first stream of the latest match received (see
	// NakamaClient.WatchReplayBuffer). Its header's delay_frames, frame_rate,
	// stream id, segment, stage (the stage's definition file) and match_info
	// (the publisher's match description, decoded), buffered_through (frames
	// received without gaps), ready (enough is buffered to start watching),
	// ended and final_frame, reset_reason. Without a header, only
	// buffered_through and ended are set.
	api.RawSetString("replayStatus", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		buffer := sys.nakama.WatchReplayBuffer()
		t := l.NewTable()
		t.RawSetString("buffered_through", lua.LNumber(buffer.BufferedThrough()))
		if header := buffer.Header(); header != nil {
			t.RawSetString("delay_frames", lua.LNumber(header.DelayFrames))
			t.RawSetString("ready", lua.LBool(liveReplayReady(buffer)))
			t.RawSetString("loading", lua.LBool(sys.nakamaSpectatorLoading))
			t.RawSetString("frame_rate", lua.LNumber(header.FrameRate))
			t.RawSetString("stream", lua.LString(header.Stream))
			t.RawSetString("segment", lua.LNumber(header.Segment))
			t.RawSetString("stage", lua.LString(header.Stage))
			if len(header.MatchInfo) > 0 {
				var info any
				if json.Unmarshal(header.MatchInfo, &info) == nil {
					t.RawSetString("match_info", toLValue(l, info))
				}
			}
		}
		if finalFrame, ended := buffer.FinalFrame(); ended {
			t.RawSetString("final_frame", lua.LNumber(finalFrame))
			t.RawSetString("ended", lua.LTrue)
		} else {
			t.RawSetString("ended", lua.LFalse)
		}
		if reason := buffer.ResetReason(); reason != "" {
			t.RawSetString("reset_reason", lua.LString(reason))
		}
		l.Push(t)
		return 1
	}))

	// startSpectatorReplay([options]) starts the live replay of the stream that
	// replayStatus describes; see nakama/README.md. It returns false while the
	// stream is not ready yet, and then shows the engine's spectator loading
	// screen and starts the replay once it is, unless options.wait is false.
	api.RawSetString("startSpectatorReplay", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		wait := true
		if l.GetTop() >= 1 && l.Get(1) != lua.LNil {
			wait = luaBoolField(tableArg(l, 1), "wait", true)
		}
		if sys.replayFile != nil {
			l.RaiseError("a replay is already active")
		}
		if sys.netConnection != nil || sys.rollback.session != nil {
			l.RaiseError("cannot start spectator replay during an active match")
		}
		buffer := sys.nakama.WatchReplayBuffer()
		header := buffer.Header()
		if header == nil {
			l.RaiseError("no live replay has been received")
		}
		if reason := buffer.ResetReason(); reason != "" {
			l.RaiseError("live replay stream was reset: %s", reason)
		}
		if !liveReplayReady(buffer) {
			if wait {
				sys.beginNakamaSpectatorLoading()
			}
			l.Push(lua.LFalse)
			return 1
		}
		rf, err := NewLiveReplayFile(buffer)
		if err != nil {
			l.RaiseError(err.Error())
		}
		// The players' games checked each other's content; a spectator's must
		// match it too, or the replay would not reproduce the match.
		if err := validateContentFingerprint(sys.currentContentFingerprint(), rf.contentFingerprint); err != nil {
			rf.Close()
			l.RaiseError("%s", err.Error())
		}
		if err := sys.beginReplaySession(rf); err != nil {
			rf.Close()
			l.RaiseError(err.Error())
		}
		sys.sessionWarning = ""
		sys.replayFile = rf
		l.Push(lua.LTrue)
		return 1
	}))

	// setReplayPublish([options]) decides how this client publishes the replay
	// stream of its next rollback matches as the netplay host: options.enabled
	// (default true), options.delay (seconds, default
	// Netplay.Rollback.ReplayBroadcastDelay) and options.info (a table sent to
	// spectators in the stream header). Without options it restores the
	// defaults.
	api.RawSetString("setReplayPublish", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		publish := NakamaReplayPublish{}
		if l.GetTop() >= 1 && l.Get(1) != lua.LNil {
			t := tableArg(l, 1)
			publish.Disabled = !luaBoolField(t, "enabled", true)
			publish.DelaySeconds = luaIntField(t, "delay", 0)
			if info := t.RawGetString("info"); info != lua.LNil {
				data, err := json.Marshal(luaRematchValueToAny(info))
				if err != nil {
					l.RaiseError("encode replay info: %v", err)
				}
				publish.Info = data
			}
		}
		sys.nakama.SetReplayPublish(publish)
		return 0
	}))

	// setReplayInfo(table) replaces only the match description of
	// setReplayPublish, for example once the characters are chosen.
	api.RawSetString("setReplayInfo", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		var info json.RawMessage
		if l.GetTop() >= 1 && l.Get(1) != lua.LNil {
			data, err := json.Marshal(luaRematchValueToAny(l.Get(1)))
			if err != nil {
				l.RaiseError("encode replay info: %v", err)
			}
			info = data
		}
		sys.nakama.SetReplayInfo(info)
		return 0
	}))

	// getAccount() reads the account. Emits "account" with payload user_id,
	// username and display_name (empty until a name has been chosen with
	// setAccountName), or with error.
	api.RawSetString("getAccount", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		n := sys.nakama
		SafeGo(func() {
			account, err := n.GetAccount(context.Background())
			n.emit(NakamaEvent{Type: "account", Payload: map[string]any{
				"user_id":      account.UserID,
				"username":     account.Username,
				"display_name": account.DisplayName,
			}, Error: err})
		})
		return 0
	}))

	// setAccountName(name) sets the player's online name, the account's
	// display name (2 to 16 characters without control characters; names
	// need not be unique). The game chooses which characters it accepts
	// before calling it (nameMissing). Emits "account_name" with
	// payload.display_name, or with error and payload.reason: invalid (the
	// server refused the name) or failed. payload.name is the name asked for.
	api.RawSetString("setAccountName", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		name := strArg(l, 1)
		n := sys.nakama
		SafeGo(func() {
			displayName, err := n.SetAccountName(context.Background(), name)
			// name: the name asked for, which tells this reply from a late
			// one to an earlier request.
			payload := map[string]any{"display_name": displayName, "name": name}
			if err != nil {
				if nakamaRPCErrorCode(err) == 3 {
					payload["reason"] = "invalid"
				} else {
					payload["reason"] = "failed"
				}
			}
			n.emit(NakamaEvent{Type: "account_name", Payload: payload, Error: err})
		})
		return 0
	}))

	// displayName() returns the player's online name as last read
	// (getAccount) or set (setAccountName), or "" before either.
	api.RawSetString("displayName", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.Push(lua.LString(""))
			return 1
		}
		l.Push(lua.LString(sys.nakama.DisplayName()))
		return 1
	}))

	// getUsers({user_id, ...}) reads other accounts. Emits "users" with
	// payload.users, a list of {user_id, username, display_name}, or with
	// error.
	api.RawSetString("getUsers", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		var ids []string
		if t, ok := l.Get(1).(*lua.LTable); ok {
			t.ForEach(func(_, v lua.LValue) {
				if s, ok := v.(lua.LString); ok && s != "" {
					ids = append(ids, string(s))
				}
			})
		}
		n := sys.nakama
		SafeGo(func() {
			users, err := n.GetUsers(context.Background(), ids)
			list := make([]any, 0, len(users))
			for _, u := range users {
				list = append(list, map[string]any{
					"user_id":      u.UserID,
					"username":     u.Username,
					"display_name": u.DisplayName,
				})
			}
			n.emit(NakamaEvent{Type: "users", Payload: map[string]any{"users": list}, Error: err})
		})
		return 0
	}))

	// nameMissing(name) returns the characters of name, each once and
	// separated by spaces, that the fight screen's name fonts cannot draw
	// (every team-mode layout's first name slot on both sides), or "" when
	// the lifebar can show the name. Online names are chosen from the
	// characters the lifebar can show. Characters outside ASCII are written
	// as code points (U+00E9), since the fonts that show the message may
	// lack them too.
	api.RawSetString("nameMissing", l.NewFunction(func(l *lua.LState) int {
		missing := lifebarNameMissing(&sys.fightScreen, strArg(l, 1))
		parts := make([]string, len(missing))
		for i, r := range missing {
			if r < 0x80 {
				parts[i] = string(r)
			} else {
				parts[i] = fmt.Sprintf("U+%04X", r)
			}
		}
		l.Push(lua.LString(strings.Join(parts, " ")))
		return 1
	}))

	// setMatchNames(name1, name2[, user1, user2]) sets the names the fight
	// screen shows for the players on sides 1 and 2 in place of the names of
	// the characters they play as (nakama_hud.go); an empty name leaves the
	// character's name. With the players' user IDs, a ranked set that
	// switches sides moves each name with its player. Without arguments it
	// clears the names. Set them for an online match or a watched match and
	// clear them afterwards; they show only in netplay, live replays and
	// match replays.
	api.RawSetString("setMatchNames", l.NewFunction(func(l *lua.LState) int {
		setOnlineMatchNames(optionalLuaString(l, 1, ""), optionalLuaString(l, 2, ""),
			optionalLuaString(l, 3, ""), optionalLuaString(l, 4, ""))
		return 0
	}))

	// matchNames() returns the names set for the players now on sides 1 and
	// 2 (after a ranked side switch, for example), "" for none. The fight
	// screen shows them only in netplay, live replays and match replays, and
	// only when the name slot's font can draw them.
	api.RawSetString("matchNames", l.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LString(onlineNameForSide(0)))
		l.Push(lua.LString(onlineNameForSide(1)))
		return 2
	}))

	// matchUsers() returns the user IDs set with the names for the players
	// now on sides 1 and 2 (after a ranked side switch, for example), "" for
	// none.
	api.RawSetString("matchUsers", l.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LString(onlineUserForSide(0)))
		l.Push(lua.LString(onlineUserForSide(1)))
		return 2
	}))

	api.RawSetString("rankedSet", l.NewFunction(func(l *lua.LState) int {
		state := l.NewTable()
		state.RawSetString("active", lua.LBool(sys.rankedSet.Active()))
		state.RawSetString("best_of", lua.LNumber(sys.rankedSet.BestOf()))
		state.RawSetString("matches", lua.LNumber(sys.rankedSet.MatchesPlayed()))
		state.RawSetString("p1_wins", lua.LNumber(sys.rankedSet.Wins(0)))
		state.RawSetString("p2_wins", lua.LNumber(sys.rankedSet.Wins(1)))
		state.RawSetString("p1_side", lua.LNumber(sys.rankedSet.SideForPlayer(0)))
		state.RawSetString("p2_side", lua.LNumber(sys.rankedSet.SideForPlayer(1)))
		state.RawSetString("p1_id", lua.LString(sys.rankedSet.players[0]))
		state.RawSetString("p2_id", lua.LString(sys.rankedSet.players[1]))
		state.RawSetString("switch_sides", lua.LBool(sys.rankedSet.Rules().SwitchSides))
		state.RawSetString("winner_keeps_selection", lua.LBool(sys.rankedSet.Rules().WinnerKeepsSelection))
		state.RawSetString("winner", lua.LNumber(sys.rankedSet.SetWinner()+1))
		state.RawSetString("last_winner", lua.LNumber(sys.rankedSet.LastWinner()+1))
		l.Push(state)
		return 1
	}))

	api.RawSetString("rankedSetRecordMatch", l.NewFunction(func(l *lua.LState) int {
		if !sys.rankedSet.Active() {
			l.Push(lua.LBool(false))
			return 1
		}
		complete := sys.rankedSet.RecordMatch(int(numArg(l, 1)))
		l.Push(lua.LBool(complete))
		return 1
	}))

	api.RawSetString("rankedSetForfeit", l.NewFunction(func(l *lua.LState) int {
		if !sys.rankedSet.Active() {
			l.Push(lua.LBool(false))
			return 1
		}
		complete := sys.rankedSet.Forfeit(int(numArg(l, 1)) - 1)
		l.Push(lua.LBool(complete))
		return 1
	}))

	api.RawSetString("rankedSetStartNextSet", l.NewFunction(func(l *lua.LState) int {
		if sys.rankedSet.StartNextSet() {
			l.Push(lua.LTrue)
		} else {
			l.Push(lua.LFalse)
		}
		return 1
	}))

	api.RawSetString("rankedSetSelectionLocked", l.NewFunction(func(l *lua.LState) int {
		player := int(numArg(l, 1)) - 1
		token := optionalLuaString(l, 2, "")
		if player < 0 || player > 1 || token == "" {
			l.Push(lua.LBool(false))
			return 1
		}
		locked := sys.rankedSet.WinnerSelectionLocked(player) && sys.rankedSet.SelectionToken(player) != "" && sys.rankedSet.SelectionToken(player) == token
		l.Push(lua.LBool(locked))
		return 1
	}))

	api.RawSetString("currentMatch", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.Push(lua.LString(""))
			return 1
		}
		l.Push(lua.LString(sys.nakama.CurrentMatchID()))
		return 1
	}))

	api.RawSetString("userId", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil || sys.nakama.Session() == nil {
			l.Push(lua.LString(""))
			return 1
		}
		l.Push(lua.LString(sys.nakama.Session().UserID))
		return 1
	}))

	api.RawSetString("username", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil || sys.nakama.Session() == nil {
			l.Push(lua.LString(""))
			return 1
		}
		l.Push(lua.LString(sys.nakama.Session().Username))
		return 1
	}))

	// localSide reports which physical side (1/2) this machine controls in the
	// current netplay session, or 0 outside netplay.
	api.RawSetString("localSide", l.NewFunction(func(l *lua.LState) int {
		side := 0
		switch {
		case sys.rollback.session != nil && sys.rollback.session.playerNo > 0:
			side = sys.rollback.session.playerNo
		case sys.netConnection != nil:
			side = 2
			if sys.netConnection.host {
				side = 1
			}
		case sys.rollback.netConnection != nil:
			side = 2
			if sys.rollback.netConnection.host {
				side = 1
			}
		}
		l.Push(lua.LNumber(side))
		return 1
	}))

	// inRollback is true while GGPO re-simulates frames; UI code with side
	// effects (network sends, menu state) must not run during re-simulation.
	api.RawSetString("inRollback", l.NewFunction(func(l *lua.LState) int {
		l.Push(lua.LBool(sys.inRollback()))
		return 1
	}))

	api.RawSetString("rematchChoice", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		action := strings.ToLower(strings.TrimSpace(strArg(l, 1)))
		if action != "rematch" && action != "select" && action != "exit" && action != "forfeit" && action != "new_set" {
			l.RaiseError("nakama.rematchChoice action must be rematch, select, exit, forfeit, or new_set")
		}
		body := map[string]any{
			"version": 1,
			"action":  action,
		}
		if round := optionalLuaInt(l, 3, 0); round > 0 {
			body["round"] = round
		}
		if l.GetTop() >= 2 && l.Get(2) != lua.LNil {
			body["snapshot"] = luaRematchValueToAny(l.Get(2))
		}
		if l.GetTop() >= 4 && l.Get(4) != lua.LNil {
			body["set_final"] = l.Get(4) == lua.LTrue
		}
		data, err := json.Marshal(body)
		if err != nil {
			l.RaiseError("encode rematch choice: %v", err)
		}
		if err := sys.nakama.SendMatchData(nakamaRematchChoiceOp, data, true); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("sendMatchData", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		op := uint32(numArg(l, 1))
		data := []byte(strArg(l, 2))
		reliable := optionalLuaBool(l, 3, false)
		if err := sys.nakama.SendMatchData(op, data, reliable); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("sendMatchDataBase64", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		op := uint32(numArg(l, 1))
		encoded := strArg(l, 2)
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			l.RaiseError("invalid base64 match data: %v", err)
		}
		reliable := optionalLuaBool(l, 3, false)
		if err := sys.nakama.SendMatchData(op, data, reliable); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("joinChat", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		target := strArg(l, 1)
		channelType := optionalLuaInt(l, 2, 1)
		persistence := optionalLuaBool(l, 3, true)
		hidden := optionalLuaBool(l, 4, false)
		if err := sys.nakama.JoinChat(target, channelType, persistence, hidden); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("sendChat", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		channelID := strArg(l, 1)
		message := strArg(l, 2)
		if err := sys.nakama.SendChat(channelID, map[string]string{"message": message}); err != nil {
			l.RaiseError(err.Error())
		}
		return 0
	}))

	api.RawSetString("listLobbies", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		// Default to lobbies only: matchmade 1v1 sessions are authoritative matches too.
		query := optionalLuaString(l, 1, "+label.kind:ikemen-lobby")
		label := optionalLuaString(l, 2, "")
		minSize := optionalLuaInt(l, 3, 1)
		maxSize := optionalLuaInt(l, 4, 8)
		limit := optionalLuaInt(l, 5, 100)
		n := sys.nakama
		SafeGo(func() {
			matches, err := n.ListMatches(context.Background(), true, query, label, minSize, maxSize, limit)
			// Each match gets label_data, its label decoded, so Lua needs no JSON parser.
			decodeLobbyLabels(matches)
			n.emit(NakamaEvent{Type: "lobby_list", Payload: matches, Raw: map[string]json.RawMessage{}, Error: err})
		})
		return 0
	}))

	api.RawSetString("createLobby", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		cfg := NakamaLobbyConfig{
			Game:       sys.nakama.cfg.Game,
			GameBuild:  sys.nakama.cfg.GameBuild,
			Format:     "queue",
			MaxPlayers: 8,
			MaxGames:   3,
			Rules:      map[string]any{},
		}
		if l.GetTop() >= 1 && l.Get(1) != lua.LNil {
			t := tableArg(l, 1)
			cfg.Name = luaStringField(t, "name", "")
			cfg.Game = luaStringField(t, "game", cfg.Game)
			cfg.GameBuild = luaStringField(t, "build", cfg.GameBuild)
			cfg.Format = luaStringField(t, "format", cfg.Format)
			cfg.MaxPlayers = luaIntField(t, "max_players", cfg.MaxPlayers)
			cfg.MaxGames = luaIntField(t, "max_games", cfg.MaxGames)
			if rules := t.RawGetString("rules"); rules != lua.LNil {
				cfg.Rules = luaTableToAny(rules)
			}
			if settings := t.RawGetString("settings"); settings != lua.LNil {
				cfg.Settings = luaTableToAny(settings)
			}
		}
		n := sys.nakama
		SafeGo(func() {
			id, code, err := n.CreateLobbyWithCode(context.Background(), cfg)
			n.emit(NakamaEvent{Type: "lobby_created", MatchID: id, Payload: map[string]any{"match_id": id, "code": code}, Error: err})
		})
		return 0
	}))

	// findLobby(code): resolves a room ID. Emits "lobby_found" with match_id
	// (or error). Joining a private lobby also needs the code:
	// nakama.joinMatch(match_id, nil, {code = code}).
	api.RawSetString("findLobby", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		code := strArg(l, 1)
		n := sys.nakama
		SafeGo(func() {
			id, err := n.FindLobby(context.Background(), code)
			n.emit(NakamaEvent{Type: "lobby_found", MatchID: id, Payload: map[string]any{"match_id": id, "code": code}, Error: err})
		})
		return 0
	}))

	// lobbyState([version]) returns the latest state of the joined lobby and
	// its version. With the version from a previous call it returns nil and
	// the same version while nothing has changed.
	api.RawSetString("lobbyState", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.Push(lua.LNil)
			l.Push(lua.LNumber(0))
			return 2
		}
		state, version, ok := sys.nakama.LobbyState()
		since := int64(optionalLuaInt(l, 1, -1))
		if !ok || version == since {
			l.Push(lua.LNil)
		} else {
			l.Push(toLValue(l, state))
		}
		l.Push(lua.LNumber(version))
		return 2
	}))

	// lobbyEvents() returns and clears the queued lobby events (chat,
	// notices, errors, kicked), oldest first.
	api.RawSetString("lobbyEvents", l.NewFunction(func(l *lua.LState) int {
		t := l.NewTable()
		if sys.nakama != nil {
			for _, ev := range sys.nakama.LobbyEvents() {
				t.Append(toLValue(l, ev))
			}
		}
		l.Push(t)
		return 1
	}))

	// lobbySend(kind[, fields]) sends a lobby command: ready, chat, member,
	// settings, start, kick, host, pair_failed, no_contest.
	api.RawSetString("lobbySend", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		kind := strArg(l, 1)
		fields := map[string]any{}
		if l.GetTop() >= 2 && l.Get(2) != lua.LNil {
			fields = luaTableToAny(l.Get(2))
		}
		if err := sys.nakama.SendLobbyCommand(kind, fields); err != nil {
			l.RaiseError("%s", err.Error())
		}
		return 0
	}))

	// ping() measures the server round trip; status().rtt_ms has the result.
	api.RawSetString("ping", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama != nil {
			_ = sys.nakama.Ping()
		}
		return 0
	}))

	api.RawSetString("getRating", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		game := optionalLuaString(l, 1, sys.nakama.cfg.Game)
		n := sys.nakama
		SafeGo(func() {
			body, err := n.GetRating(context.Background(), game)
			n.emit(NakamaEvent{Type: "rating", Payload: json.RawMessage(body), Error: err})
		})
		return 0
	}))

	api.RawSetString("submitResult", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		req := NakamaResultRequest{
			MatchID:    strArg(l, 1),
			OpponentID: strArg(l, 2),
			Game:       optionalLuaString(l, 3, sys.nakama.cfg.Game),
			Result:     int(numArg(l, 4)),
			ReplayHash: optionalLuaString(l, 5, ""),
		}
		if req.Result != 0 && req.Result != 1 {
			l.RaiseError("nakama.submitResult result must be 0 or 1")
		}
		n := sys.nakama
		SafeGo(func() {
			body, err := n.SubmitResult(context.Background(), req)
			n.emit(NakamaEvent{Type: "result", Payload: json.RawMessage(body), Error: err})
		})
		return 0
	}))

	api.RawSetString("reportLobbyResult", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		winner := strArg(l, 1)
		loser := strArg(l, 2)
		var err error
		if l.GetTop() >= 3 && l.Get(3) != lua.LNil {
			// The pairing's seq from lobby state: a late report cannot land on a newer pairing.
			err = sys.nakama.ReportLobbyResultSeq(winner, loser, int64(numArg(l, 3)))
		} else {
			err = sys.nakama.ReportLobbyResult(winner, loser)
		}
		if err != nil {
			l.RaiseError("%s", err.Error())
		}
		return 0
	}))

	l.SetGlobal("nakama", api)
}

func optionalLuaString(l *lua.LState, index int, fallback string) string {
	if index < 1 || index > l.GetTop() || l.Get(index) == lua.LNil {
		return fallback
	}
	return l.ToString(index)
}

func optionalLuaInt(l *lua.LState, index, fallback int) int {
	if index < 1 || index > l.GetTop() || l.Get(index) == lua.LNil {
		return fallback
	}
	if n, ok := l.Get(index).(lua.LNumber); ok {
		return int(n)
	}
	return fallback
}

func optionalLuaBool(l *lua.LState, index int, fallback bool) bool {
	if index < 1 || index > l.GetTop() || l.Get(index) == lua.LNil {
		return fallback
	}
	return l.ToBool(index)
}

func luaStringField(t *lua.LTable, key, fallback string) string {
	v := t.RawGetString(key)
	if v == lua.LNil {
		return fallback
	}
	return v.String()
}

func luaRematchTableToAny(value *lua.LTable) any {
	if value == nil {
		return nil
	}
	numeric := map[int]any{}
	stringsMap := map[string]any{}
	maxIndex := 0
	hasNonPositiveNumeric := false
	value.ForEach(func(key, val lua.LValue) {
		switch key := key.(type) {
		case lua.LNumber:
			i := int(key)
			if lua.LNumber(i) != key || i < 1 {
				hasNonPositiveNumeric = true
				return
			}
			numeric[i] = luaRematchValueToAny(val)
			if i > maxIndex {
				maxIndex = i
			}
		case lua.LString:
			stringsMap[string(key)] = luaRematchValueToAny(val)
		}
	})
	if len(stringsMap) == 0 && !hasNonPositiveNumeric && maxIndex > 0 && len(numeric) == maxIndex {
		arr := make([]any, maxIndex)
		for i := 1; i <= maxIndex; i++ {
			arr[i-1] = numeric[i]
		}
		return arr
	}
	for i, v := range numeric {
		stringsMap[strconv.Itoa(i)] = v
	}
	return stringsMap
}

func luaRematchValueToAny(value lua.LValue) any {
	switch value.Type() {
	case lua.LTString:
		return value.String()
	case lua.LTNumber:
		if n, ok := value.(lua.LNumber); ok {
			return float64(n)
		}
		return float64(0)
	case lua.LTBool:
		if b, ok := value.(lua.LBool); ok {
			return bool(b)
		}
		return false
	case lua.LTTable:
		return luaRematchTableToAny(value.(*lua.LTable))
	default:
		return value.String()
	}
}

func luaIntField(t *lua.LTable, key string, fallback int) int {
	v := t.RawGetString(key)
	if n, ok := v.(lua.LNumber); ok {
		return int(n)
	}
	return fallback
}

func luaBoolField(t *lua.LTable, key string, fallback bool) bool {
	v := t.RawGetString(key)
	if v == lua.LNil {
		return fallback
	}
	if b, ok := v.(lua.LBool); ok {
		return bool(b)
	}
	return fallback
}

func luaTableToAny(value lua.LValue) map[string]any {
	t, ok := value.(*lua.LTable)
	if !ok {
		return map[string]any{}
	}
	out := map[string]any{}
	t.ForEach(func(key, value lua.LValue) {
		if key.Type() != lua.LTString {
			return
		}
		out[key.String()] = luaValueToAny(value)
	})
	return out
}

func luaValueToAny(value lua.LValue) any {
	switch value.Type() {
	case lua.LTString:
		return value.String()
	case lua.LTNumber:
		if n, ok := value.(lua.LNumber); ok {
			return float64(n)
		}
		return float64(0)
	case lua.LTBool:
		if b, ok := value.(lua.LBool); ok {
			return bool(b)
		}
		return false
	case lua.LTTable:
		return luaTableToAny(value)
	default:
		return value.String()
	}
}

func nakamaDispatchLua(l *lua.LState, callbacks *nakamaLuaHandlers, ev NakamaEvent) {
	// Matchmaker results are immediately joined into the Nakama relay match. The
	// relay is only the rendezvous/signalling/replay channel; GGPO gameplay remains
	// separate and will use the P2P transport once that layer is attached.
	if ev.Type == "matchmaker_matched" && ev.Matchmaker != nil && sys.nakama != nil {
		// The matched ticket contains the negotiated rules; capture them before joining.
		if len(ev.Matchmaker.Users) >= 2 {
			var players [2]string
			players[0] = ev.Matchmaker.Users[0].Presence.UserID
			players[1] = ev.Matchmaker.Users[1].Presence.UserID
			props := ev.Matchmaker.Users[0].Properties
			mode := "unranked"
			if v, ok := props["mode"].(string); ok && v != "" {
				mode = v
			}
			if mode == "ranked" {
				rules := RankedSetRules{BestOf: gameOptionInt("Netplay.Ranked.BestOf"), SwitchSides: gameOptionBool("Netplay.Ranked.SwitchSides"), WinnerKeepsSelection: gameOptionBool("Netplay.Ranked.WinnerKeepsSelection")}
				if v, ok := props["ranked_best_of"].(float64); ok {
					rules.BestOf = int(v)
				} else if v, ok := props["ranked_best_of"].(string); ok {
					if n, err := strconv.Atoi(v); err == nil {
						rules.BestOf = n
					}
				}
				if v, ok := props["ranked_switch_sides"].(string); ok {
					rules.SwitchSides = strings.EqualFold(v, "true")
				}
				if v, ok := props["ranked_winner_keeps_selection"].(string); ok {
					rules.WinnerKeepsSelection = strings.EqualFold(v, "true")
				}
				sys.rankedSet.Begin(players, rules)
			} else {
				sys.rankedSet.Reset()
			}
		}
		if err := sys.nakama.JoinMatch(ev.Matchmaker.MatchID, ev.Matchmaker.Token); err != nil {
			nakamaDispatchLuaEvent(l, callbacks, NakamaEvent{Type: "error", Error: err})
		}
	}
	if ev.Type == "match_joined" && sys.rollback.session != nil && sys.nakama != nil {
		sys.rollback.session.replayStream.SetSink(&nakamaReplaySink{client: sys.nakama})
	}
	if ev.Type == "match_data" && ev.MatchData != nil && uint32(ev.MatchData.OpCode) == nakamaSignalOp {
		// The signal was already applied by the network goroutine (NakamaClient.deliverSignal).
		nakamaDispatchLuaEvent(l, callbacks, NakamaEvent{Type: "p2p_signal", MatchID: ev.MatchID, MatchData: ev.MatchData})
	}
	nakamaDispatchLuaEvent(l, callbacks, ev)
}

func nakamaDispatchLuaEvent(l *lua.LState, callbacks *nakamaLuaHandlers, ev NakamaEvent) {
	fns := callbacks.forEvent(ev.Type)
	if len(fns) == 0 {
		return
	}
	args := []lua.LValue{lua.LString(ev.Type)}
	t := l.NewTable()
	t.RawSetString("type", lua.LString(ev.Type))
	if ev.Ticket != "" {
		t.RawSetString("ticket", lua.LString(ev.Ticket))
	}
	if ev.MatchID != "" {
		t.RawSetString("match_id", lua.LString(ev.MatchID))
	}
	if ev.MatchToken != "" {
		t.RawSetString("token", lua.LString(ev.MatchToken))
	}
	if ev.ChannelID != "" {
		t.RawSetString("channel_id", lua.LString(ev.ChannelID))
	}
	if ev.Error != nil {
		t.RawSetString("error", lua.LString(ev.Error.Error()))
	}
	if ev.Matchmaker != nil {
		t.RawSetString("matchmaker", toLValue(l, ev.Matchmaker))
	}
	if ev.MatchData != nil {
		t.RawSetString("match_data", toLValue(l, ev.MatchData))
	}
	if ev.ChannelMessage != nil {
		t.RawSetString("chat", toLValue(l, ev.ChannelMessage))
	}
	if ev.Payload != nil {
		t.RawSetString("payload", toLValue(l, ev.Payload))
	}
	args = append(args, t)
	for _, fn := range fns {
		if err := l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, args...); err != nil {
			log.Printf("Nakama Lua event handler %q failed: %v", ev.Type, err)
		}
	}
}
