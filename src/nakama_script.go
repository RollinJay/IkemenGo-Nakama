package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
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

func nakamaScriptInit(l *lua.LState) {
	api := l.NewTable()
	callbacks := make(map[string]*lua.LFunction)

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
			task := func() { nakamaDispatchLua(l, callbacks, ev) }
			select {
			case sys.mainThreadTask <- task:
			case <-n.closed:
				return
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

	api.RawSetString("on", l.NewFunction(func(l *lua.LState) int {
		name := strArg(l, 1)
		fn, ok := l.Get(2).(*lua.LFunction)
		if !ok {
			l.RaiseError("nakama.on requires a function")
		}
		callbacks[name] = fn
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
			for _, key := range []string{"game", "build"} {
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

	api.RawSetString("p2pReady", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.Push(lua.LFalse)
			return 1
		}
		l.Push(lua.LBool(sys.nakama.P2PReady()))
		return 1
	}))

	api.RawSetString("replayStatus", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		buffer := sys.nakama.ReplayBuffer()
		t := l.NewTable()
		t.RawSetString("buffered_through", lua.LNumber(buffer.BufferedThrough()))
		if header := buffer.Header(); header != nil {
			ready := buffer.BufferedThrough() >= header.DelayFrames
			t.RawSetString("delay_frames", lua.LNumber(header.DelayFrames))
			t.RawSetString("ready", lua.LBool(ready))
			t.RawSetString("loading", lua.LBool(sys.nakamaSpectatorLoading))
			t.RawSetString("frame_rate", lua.LNumber(header.FrameRate))
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

	api.RawSetString("startSpectatorReplay", l.NewFunction(func(l *lua.LState) int {
		if sys.nakama == nil {
			l.RaiseError("nakama is not connected")
		}
		if sys.replayFile != nil {
			l.RaiseError("a replay is already active")
		}
		if sys.netConnection != nil || sys.rollback.session != nil {
			l.RaiseError("cannot start spectator replay during an active match")
		}
		buffer := sys.nakama.ReplayBuffer()
		header := buffer.Header()
		if header == nil {
			l.RaiseError("no live replay has been received")
		}
		if reason := buffer.ResetReason(); reason != "" {
			l.RaiseError("live replay stream was reset: %s", reason)
		}
		if buffer.BufferedThrough() < header.DelayFrames {
			sys.beginNakamaSpectatorLoading()
			l.Push(lua.LFalse)
			return 1
		}
		rf, err := NewLiveReplayFile(buffer)
		if err != nil {
			l.RaiseError(err.Error())
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
		query := optionalLuaString(l, 1, "")
		label := optionalLuaString(l, 2, "")
		minSize := optionalLuaInt(l, 3, 1)
		maxSize := optionalLuaInt(l, 4, 8)
		limit := optionalLuaInt(l, 5, 100)
		n := sys.nakama
		SafeGo(func() {
			matches, err := n.ListMatches(context.Background(), true, query, label, minSize, maxSize, limit)
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
		}
		n := sys.nakama
		SafeGo(func() {
			id, err := n.CreateLobby(context.Background(), cfg)
			n.emit(NakamaEvent{Type: "lobby_created", MatchID: id, Error: err})
		})
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
		if err := sys.nakama.ReportLobbyResult(winner, loser); err != nil {
			l.RaiseError(err.Error())
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

func nakamaDispatchLua(l *lua.LState, callbacks map[string]*lua.LFunction, ev NakamaEvent) {
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
	if ev.Type == "match_data" && ev.MatchData != nil && ev.MatchData.OpCode == int64(nakamaSignalOp) {
		if sys.nakama != nil && sys.nakama.P2P() != nil {
			var signal P2PSignal
			if err := json.Unmarshal(ev.MatchData.BinaryData, &signal); err != nil {
				nakamaDispatchLuaEvent(l, callbacks, NakamaEvent{Type: "p2p_error", MatchID: ev.MatchID, Error: err})
			} else if err := sys.nakama.P2P().HandleSignal(signal); err != nil {
				nakamaDispatchLuaEvent(l, callbacks, NakamaEvent{Type: "p2p_error", MatchID: ev.MatchID, Error: err})
			}
		}
		nakamaDispatchLuaEvent(l, callbacks, NakamaEvent{Type: "p2p_signal", MatchID: ev.MatchID, MatchData: ev.MatchData})
	}
	nakamaDispatchLuaEvent(l, callbacks, ev)
}

func nakamaDispatchLuaEvent(l *lua.LState, callbacks map[string]*lua.LFunction, ev NakamaEvent) {
	fn := callbacks[ev.Type]
	if fn == nil {
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
	if err := l.CallByParam(lua.P{Fn: fn, NRet: 0, Protect: true}, args...); err != nil {
		log.Printf("Nakama Lua event handler %q failed: %v", ev.Type, err)
	}
}
