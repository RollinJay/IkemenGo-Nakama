local nk = require("nakama")
local ranked = require("ikemen_ranked")

-- IKEMEN GO matchmaking/session coordination match.
-- This match is a Nakama relay for P2P signaling and delayed replay chunks.
-- It does not carry the GGPO gameplay stream.
--
-- In a ranked session (mode "ranked"), it also follows the session's ranked
-- sets (ikemen_ranked.lua) and keeps both players' replay streams instead of
-- relaying them: each player's game publishes its stream in a ranked session.
-- A player's replay of a set is stored when the set has been decided and the
-- player's last stream of it has ended, when the player leaves, and when the
-- session ends. The matchmaker records the session's first set; when both
-- players choose to play another set, this match records the next one. A
-- player who leaves before the set is decided is noted on the set's record.
-- What it keeps of a player's streams is limited per set
-- (IKEMEN_RANKED_REPLAY_MAX_BYTES) and grows no faster than play makes it
-- (REPLAY_RATE); a stream is kept only as it continues, frame after frame.

local MAX_PLAYERS = 8
local OP_REMATCH_CHOICE = 1229673027
local OP_REMATCH_DECISION = 1229673028

-- Replay stream messages (src/nakama_client.go) and the largest of each
-- kept: a header is about 6 KB (more with the players' start state), a
-- chunk of 30 frames under 3 KB, and an end in a ranked session carries the
-- list of the match's files.
local OP_REPLAY_HEADER = 0x494B5210
local OP_REPLAY_CHUNK = 0x494B5211
local OP_REPLAY_END = 0x494B5212
local OP_REPLAY_RESET = 0x494B5213
local REPLAY_HEADER_MAX = 32 * 1024
local REPLAY_CHUNK_MAX = 8 * 1024
local REPLAY_END_MAX = 32 * 1024
-- The most frames a chunk holds (replayStreamChunkFrames in
-- src/replay_stream.go), and the most bytes a frame of inputs has.
local REPLAY_CHUNK_FRAMES = 30
local REPLAY_FRAME_BYTES_MAX = 1024
-- What the session keeps of a player's streams grows no faster than play
-- makes them: at most REPLAY_ALLOWANCE bytes, plus REPLAY_RATE bytes for
-- each second the session has lasted, over all its sets. Play publishes
-- about 5.3 KB a second, and a header and an end of about 9 KB per match.
local REPLAY_ALLOWANCE = 256 * 1024
local REPLAY_RATE = 16 * 1024

local function is_replay_op(op)
    return op == OP_REPLAY_HEADER or op == OP_REPLAY_CHUNK or op == OP_REPLAY_END or op == OP_REPLAY_RESET
end

-- The context the ikemen_ranked functions read settings from.
local function settings(state)
    return { env = state.env }
end

-- Runs fn(...), a call into ikemen_ranked that reads or writes storage: an
-- error (the database failed) is logged instead of stopping the session,
-- which would drop its players and the replays it holds.
local function guarded(what, fn, ...)
    local ok, err = pcall(fn, ...)
    if not ok then
        nk.logger_error(string.format("ikemen ranked: %s failed: %s", what, tostring(err)))
    end
end

local function set_id(state, set)
    return ranked.set_id(state.match_id, set or state.set_no)
end

-- Stores a player's replay of a set: complete when the set was decided, each
-- stream ended and the player did not leave before the set was decided.
local function store_replay(state, user_id)
    local buf = state.replay_bufs[user_id]
    state.replay_bufs[user_id] = nil
    if buf == nil or #buf.streams == 0 then
        return
    end
    local complete = state.decided[buf.set] == true and not buf.left_early and not buf.truncated
    local streams = {}
    for _, s in ipairs(buf.streams) do
        if s.final == nil then
            complete = false
        end
        streams[#streams + 1] = { header = s.header, chunks = s.chunks, final = s.final, reset = s.reset, at = s.at,
            frames = s.frames }
    end
    guarded("storing the replay of " .. user_id, ranked.store_replay, settings(state), set_id(state, buf.set),
        user_id, state.usernames[user_id], { streams = streams, bytes = buf.bytes, truncated = buf.truncated }, complete)
end

-- The current set is decided: a player's choice after its last match said so,
-- a forfeit, both players' choice of another set, or both players' claims
-- (one claim alone does not decide it: a player can claim at any time). Each
-- player's replay of the set is stored once their last stream of it has
-- ended.
local function set_decided(state)
    if not state.ranked or state.decided[state.set_no] then
        return
    end
    state.decided[state.set_no] = true
    for user_id, buf in pairs(state.replay_bufs) do
        if buf.set == state.set_no and buf.open == nil then
            store_replay(state, user_id)
        end
    end
end

-- Records when the current set ended (once).
local function note_set_end(state)
    local now = ranked.now()
    guarded("recording the end of " .. set_id(state), ranked.update_set, set_id(state), function(v)
        if v.ended ~= nil then
            return false
        end
        v.ended = now
        return true
    end)
end

-- Both players chose "Play Another Set": the finished set has ended, and the
-- next one is recorded. The finished set's last streams can still arrive:
-- they belong to it, as do all streams that started before this.
local function start_next_set(state)
    set_decided(state)
    note_set_end(state)
    state.set_no = state.set_no + 1
    guarded("recording " .. set_id(state), ranked.create_set, state.match_id, state.set_no, {
        game = state.game,
        build = state.build,
        best_of = state.ranked_best_of,
        players = state.order,
    })
end

-- A player left before the current set was decided: the set's record says
-- so (moderators decide such sets).
local function note_left_early(state, user_id)
    local buf = state.replay_bufs[user_id]
    if buf ~= nil then
        buf.left_early = true
    end
    guarded("noting that " .. user_id .. " left " .. set_id(state), ranked.update_set, set_id(state), function(v)
        for _, uid in ipairs(v.left_early) do
            if uid == user_id then
                return false
            end
        end
        v.left_early[#v.left_early + 1] = user_id
        return true
    end)
end

-- The session ends: every replay kept is stored, and the current set has
-- ended (its claims are due within IKEMEN_RANKED_CLAIM_MINUTES).
local function finish(state)
    if not state.ranked or state.finished then
        return
    end
    state.finished = true
    for user_id in pairs(state.replay_bufs) do
        store_replay(state, user_id)
    end
    note_set_end(state)
end

-- A match of the current set has started: the set's record says so (a set
-- that nobody claims is void unless a match was played and a player left
-- early, see ikemen_ranked.lua).
local function note_played(state)
    if state.played[state.set_no] then
        return
    end
    state.played[state.set_no] = true
    guarded("noting a match of " .. set_id(state), ranked.update_set, set_id(state), function(v)
        if v.played == true then
            return false
        end
        v.played = true
        return true
    end)
end

-- Whether size more bytes of a player's streams can be kept: within the
-- set's limit (IKEMEN_RANKED_REPLAY_MAX_BYTES) and within what the session
-- has allowed the player so far (REPLAY_ALLOWANCE, REPLAY_RATE).
local function fits(state, user_id, buf, size)
    if buf.bytes + size > state.replay_max_bytes then
        return false
    end
    local seconds = math.max(0, (nk.time() - state.started) / 1000)
    return (state.kept[user_id] or 0) + size <= REPLAY_ALLOWANCE + REPLAY_RATE * seconds
end

local function keep(state, user_id, buf, size)
    buf.bytes = buf.bytes + size
    state.kept[user_id] = (state.kept[user_id] or 0) + size
end

-- A stream that takes no more messages: it ended, it was withdrawn, or what
-- came next did not continue it (reason). The player's replay of the set is
-- stored once the set is decided and none of its streams is still going.
local function close_stream(state, user_id, buf, s, reason)
    if reason ~= nil and s.final == nil and s.reset == nil then
        s.reset = reason
    end
    if buf.open == s then
        buf.open = nil
    end
    if buf.open == nil and state.decided[buf.set] then
        store_replay(state, user_id)
    end
end

-- A message the limits leave out cuts the player's replay of the set there:
-- its stream ends at its last chunk kept, and nothing more of the set is
-- kept for the player (a replay with a hole in it would stop at the hole).
local function cut(state, user_id, buf, s)
    buf.truncated = true
    state.cut[user_id] = buf.set
    if s ~= nil then
        close_stream(state, user_id, buf, s, "byte_limit")
    end
end

-- Keeps a replay stream message of one of the two players (ranked sessions).
local function keep_stream(state, message)
    local sender = message.sender and message.sender.user_id or ""
    if sender == "" or not state.expected[sender] then
        return
    end
    local op = message.op_code
    local data = message.data or ""
    local buf = state.replay_bufs[sender]
    if op == OP_REPLAY_HEADER then
        if #data > REPLAY_HEADER_MAX then
            return
        end
        local ok, header = pcall(nk.json_decode, data)
        if not ok or type(header) ~= "table" or type(header.stream) ~= "string" or header.stream == "" then
            return
        end
        note_played(state)
        if state.replay_max_bytes <= 0 then
            return
        end
        -- The bytes of a frame of inputs, which each chunk's payload has.
        local frame_bytes = (tonumber(header.input_count) or 0) * (tonumber(header.input_bytes) or 0)
        if frame_bytes < 1 or frame_bytes > REPLAY_FRAME_BYTES_MAX or frame_bytes ~= math.floor(frame_bytes) then
            return
        end
        if buf ~= nil and buf.set ~= state.set_no then
            -- The first stream of a new set: the player's replay of the
            -- previous one is done.
            store_replay(state, sender)
            buf = nil
        end
        if state.cut[sender] == state.set_no then
            return
        end
        if buf == nil then
            buf = { set = state.set_no, streams = {}, by_id = {}, bytes = 0, truncated = false, left_early = false }
            state.replay_bufs[sender] = buf
        end
        if buf.by_id[header.stream] ~= nil then
            return
        end
        if not fits(state, sender, buf, #data) then
            cut(state, sender, buf, nil)
            return
        end
        local s = { id = header.stream, header = data, chunks = {}, at = ranked.now(), frames = 0, frame_bytes = frame_bytes }
        buf.streams[#buf.streams + 1] = s
        buf.by_id[header.stream] = s
        buf.open = s
        keep(state, sender, buf, #data)
        return
    end
    if buf == nil or buf.truncated then
        return
    end
    if op == OP_REPLAY_CHUNK then
        -- The chunk is kept as it came, and becomes a frames record of the
        -- replay file (ikemen_ranked.lua): it must be a chunk of the stream.
        if #data > REPLAY_CHUNK_MAX or string.sub(data, 1, 1) ~= "{" then
            return
        end
        local ok, chunk = pcall(nk.json_decode, data)
        if not ok or type(chunk) ~= "table" or type(chunk.payload) ~= "string"
            or tonumber(chunk.start_frame) == nil or tonumber(chunk.frame_count) == nil then
            return
        end
        local s = buf.by_id[tostring(chunk.stream or "")]
        if s == nil or s.final ~= nil or s.reset ~= nil then
            return
        end
        -- It continues the frames kept, with at most a chunk's frames, each
        -- with its inputs (base64): otherwise the stream ends where it was.
        local count = tonumber(chunk.frame_count)
        if tonumber(chunk.start_frame) ~= s.frames or count < 1 or count > REPLAY_CHUNK_FRAMES or count ~= math.floor(count)
            or #chunk.payload ~= 4 * math.ceil(count * s.frame_bytes / 3) then
            close_stream(state, sender, buf, s, "broken_stream")
            return
        end
        if not fits(state, sender, buf, #data) then
            cut(state, sender, buf, s)
            return
        end
        s.chunks[#s.chunks + 1] = data
        s.frames = s.frames + count
        keep(state, sender, buf, #data)
        return
    end
    if #data > REPLAY_END_MAX then
        return
    end
    local ok, body = pcall(nk.json_decode, data)
    if not ok or type(body) ~= "table" then
        return
    end
    local s = buf.by_id[tostring(body.stream or "")]
    if s == nil or s.final ~= nil or s.reset ~= nil then
        return
    end
    if op == OP_REPLAY_END and tonumber(body.final_frame) ~= nil then
        if tonumber(body.final_frame) ~= s.frames then
            -- An end that is not where the frames kept end.
            close_stream(state, sender, buf, s, "broken_stream")
        elseif not fits(state, sender, buf, #data) then
            -- No room for the end: the stream stops at its last chunk.
            cut(state, sender, buf, s)
        else
            s.final = data
            keep(state, sender, buf, #data)
            close_stream(state, sender, buf, s, nil)
        end
    else
        close_stream(state, sender, buf, s, string.sub(tostring(body.reason or "reset"), 1, 128))
    end
end

local function rematch_decision(dispatcher, state, action, snapshot, source_user_id, reason)
    if state.ranked and (action == "forfeit" or action == "new_set") then
        set_decided(state)
    end
    local payload = {
        kind = "ikemen-rematch-decision",
        version = 1,
        round = state.rematch_round,
        action = action,
        source_user_id = source_user_id or "",
        reason = reason or "",
    }
    if snapshot ~= nil then
        payload.snapshot = snapshot
    end
    dispatcher.broadcast_message(OP_REMATCH_DECISION, nk.json_encode(payload), nil, nil)
    state.rematch_round = state.rematch_round + 1
    state.rematch_choices = {}
    state.rematch_set_final = false
    if state.ranked and action == "new_set" then
        start_next_set(state)
    end
end

local function match_init(context, params)
    params = params or {}
    local state = {
        mode = params.mode or "unranked",
        game = params.game or "",
        build = params.build or "",
        ranked_best_of = params.ranked_best_of or "3",
        ranked_switch_sides = params.ranked_switch_sides or "false",
        ranked_winner_keeps_selection = params.ranked_winner_keeps_selection or "false",
        players = {},
        expected = {},
        rematch_round = 1,
        rematch_choices = {},
        rematch_set_final = false,
        departed = {},
        empty_ticks = 0,
        -- Ranked sessions (see the top of this file).
        match_id = context.match_id,
        env = context.env,
        ranked = (params.mode or "unranked") == "ranked",
        set_no = 1,
        decided = {},
        played = {},
        replay_bufs = {},
        replay_max_bytes = ranked.replay_days(context) > 0 and ranked.replay_max_bytes(context) or 0,
        -- When the session began (milliseconds), the bytes of each player's
        -- streams kept so far over all its sets, and the set whose replay
        -- the limits cut, by user id.
        started = nk.time(),
        kept = {},
        cut = {},
        order = {},
        usernames = {},
        finished = false,
    }
    for _, entry in ipairs(params.expected_users or {}) do
        if entry.presence and entry.presence.user_id then
            state.expected[entry.presence.user_id] = true
            state.order[#state.order + 1] = { user_id = entry.presence.user_id, username = entry.presence.username or "" }
            state.usernames[entry.presence.user_id] = entry.presence.username or ""
        end
    end
    return state, 10, nk.json_encode({
        kind = "ikemen-session",
        game = state.game,
        build = state.build,
        mode = state.mode,
        ranked_best_of = state.ranked_best_of,
        ranked_switch_sides = state.ranked_switch_sides,
        ranked_winner_keeps_selection = state.ranked_winner_keeps_selection,
        max_players = MAX_PLAYERS,
    })
end

local function player_count(state)
    local count = 0
    for _ in pairs(state.players) do
        count = count + 1
    end
    return count
end

local function valid_integer(value, minimum)
    local n = tonumber(value)
    return n ~= nil and n == math.floor(n) and n >= (minimum or 0)
end

local function valid_rematch_team(team)
    if type(team) ~= "table" or #team < 1 then
        return false
    end
    for i = 1, #team do
        local selection = team[i]
        if type(selection) ~= "table" or not valid_integer(selection.ref, 1) or not valid_integer(selection.pal, 1) then
            return false
        end
    end
    return true
end

local function valid_rematch_snapshot(snapshot)
    if type(snapshot) ~= "table" or tonumber(snapshot.version or 0) ~= 1 then
        return false
    end
    if not valid_integer(snapshot.stageNo, 1) then
        return false
    end
    if type(snapshot.p1teammode) ~= "string" or type(snapshot.p2teammode) ~= "string" then
        return false
    end
    return valid_rematch_team(snapshot.p1) and valid_rematch_team(snapshot.p2)
end

local function same_rematch_snapshot(a, b)
    if not valid_rematch_snapshot(a) or not valid_rematch_snapshot(b) then
        return false
    end
    if tonumber(a.stageNo) ~= tonumber(b.stageNo) or a.p1teammode ~= b.p1teammode or a.p2teammode ~= b.p2teammode then
        return false
    end
    for side = 1, 2 do
        local key = "p" .. side
        if #a[key] ~= #b[key] then
            return false
        end
        for i = 1, #a[key] do
            if tonumber(a[key][i].ref) ~= tonumber(b[key][i].ref) or tonumber(a[key][i].pal) ~= tonumber(b[key][i].pal) then
                return false
            end
        end
    end
    return true
end

local function match_join_attempt(context, dispatcher, tick, state, presence, metadata)
    if player_count(state) >= MAX_PLAYERS then
        return state, false, "Session is full"
    end
    if next(state.expected) ~= nil and not state.expected[presence.user_id] then
        return state, false, "User was not matched into this IKEMEN session"
    end
    return state, true
end

local function match_join(context, dispatcher, tick, state, presences)
    for _, presence in ipairs(presences) do
        state.players[presence.user_id] = {
            session_id = presence.session_id,
            username = presence.username,
            presence = presence,
        }
        state.departed[presence.user_id] = nil
    end
    return state
end

-- Relay to everyone except the sender, and keep the sender presence on the
-- message so clients can tell who published it.
local function relay(dispatcher, state, message)
    local sender_id = message.sender and message.sender.user_id or ""
    local targets = {}
    for user_id, player in pairs(state.players) do
        if user_id ~= sender_id and player.presence ~= nil then
            targets[#targets + 1] = player.presence
        end
    end
    if #targets > 0 then
        dispatcher.broadcast_message(message.op_code, message.data, targets, message.sender)
    end
end

-- A 1v1 post-match decision cannot complete once the opponent has left.
local function resolve_departure(dispatcher, state, reason)
    local departed_id = next(state.departed)
    if state.mode == "ranked" and not state.rematch_set_final and departed_id ~= nil then
        rematch_decision(dispatcher, state, "forfeit", nil, departed_id, reason)
    else
        rematch_decision(dispatcher, state, "exit", nil, departed_id or "", reason)
    end
end

local function match_leave(context, dispatcher, tick, state, presences)
    local leaving_id = nil
    for _, presence in ipairs(presences) do
        if leaving_id == nil then
            leaving_id = presence.user_id
        end
        state.players[presence.user_id] = nil
        state.rematch_choices[presence.user_id] = nil
        state.departed[presence.user_id] = true
        if state.ranked then
            -- Only a player who leaves the other behind left early: the other
            -- player's session ends with it.
            if not state.decided[state.set_no] and player_count(state) > 0 then
                note_left_early(state, presence.user_id)
            end
            store_replay(state, presence.user_id)
        end
    end
    local count = 0
    for _ in pairs(state.players) do
        count = count + 1
    end
    if count == 0 then
        finish(state)
        return nil
    end
    local outstanding = false
    for _ in pairs(state.rematch_choices) do
        outstanding = true
        break
    end
    if outstanding then
        if state.mode == "ranked" and not state.rematch_set_final and leaving_id ~= nil then
            rematch_decision(dispatcher, state, "forfeit", nil, leaving_id, "opponent_left")
        else
            rematch_decision(dispatcher, state, "exit", nil, "", "opponent_left")
        end
    end
    return state
end

local function match_loop(context, dispatcher, tick, state, messages)
    for _, message in ipairs(messages) do
        if message.op_code == OP_REMATCH_CHOICE then
            local sender_id = message.sender and message.sender.user_id or ""
            if sender_id ~= "" and state.players[sender_id] ~= nil then
                local ok, command = pcall(nk.json_decode, message.data or "{}")
                if ok and type(command) == "table" and tonumber(command.version or 0) == 1 then
                    local round = tonumber(command.round or state.rematch_round) or state.rematch_round
                    local action = tostring(command.action or "")
                    if round == state.rematch_round and (action == "rematch" or action == "select" or action == "exit" or action == "forfeit" or action == "new_set") then
                        state.rematch_set_final = state.rematch_set_final or command.set_final == true
                        if command.set_final == true then
                            set_decided(state)
                        end
                        if action == "exit" then
                            rematch_decision(dispatcher, state, "exit", nil, sender_id, "player_exit")
                        elseif action == "forfeit" then
                            if state.mode == "ranked" and not state.rematch_set_final then
                                rematch_decision(dispatcher, state, "forfeit", nil, sender_id, "player_forfeit")
                            else
                                rematch_decision(dispatcher, state, "exit", nil, sender_id, "invalid_forfeit_action")
                            end
                        elseif action == "select" then
                            if state.rematch_set_final then
                                rematch_decision(dispatcher, state, "exit", nil, sender_id, "invalid_final_action")
                            else
                                rematch_decision(dispatcher, state, "select", nil, sender_id, "player_select")
                            end
                        elseif action == "new_set" then
                            -- Starting another ranked set is a two-party decision.
                            -- An exit/forfeit from either side still resolves immediately.
                            if state.mode == "ranked" and state.rematch_set_final then
                                state.rematch_choices[sender_id] = true
                                if player_count(state) < 2 then
                                    resolve_departure(dispatcher, state, "opponent_left")
                                    return state
                                end
                                local choice_count = 0
                                for _ in pairs(state.rematch_choices) do
                                    choice_count = choice_count + 1
                                end
                                if choice_count >= 2 then
                                    rematch_decision(dispatcher, state, "new_set", nil, sender_id, "both_confirmed")
                                end
                            else
                                rematch_decision(dispatcher, state, "exit", nil, sender_id, "invalid_new_set_action")
                            end
                        elseif action == "rematch" then
                            if state.rematch_set_final then
                                rematch_decision(dispatcher, state, "exit", nil, sender_id, "invalid_final_action")
                            elseif not valid_rematch_snapshot(command.snapshot) then
                                rematch_decision(dispatcher, state, "select", nil, sender_id, "invalid_snapshot")
                            else
                                state.rematch_choices[sender_id] = command.snapshot
                                if player_count(state) < 2 then
                                    resolve_departure(dispatcher, state, "opponent_left")
                                    return state
                                end
                                local choice_count = 0
                                local ids = {}
                                for user_id in pairs(state.rematch_choices) do
                                    choice_count = choice_count + 1
                                    ids[#ids + 1] = user_id
                                end
                                if choice_count >= 2 then
                                    table.sort(ids)
                                    local first_snapshot = state.rematch_choices[ids[1]]
                                    local second_snapshot = state.rematch_choices[ids[2]]
                                    if same_rematch_snapshot(first_snapshot, second_snapshot) then
                                        rematch_decision(dispatcher, state, "rematch", first_snapshot, ids[1], "both_confirmed")
                                    else
                                        -- The VS state must agree on both peers. A mismatch is
                                        -- treated as a safe return to Character Select rather
                                        -- than allowing one peer to dictate a different game.
                                        rematch_decision(dispatcher, state, "select", nil, sender_id, "snapshot_mismatch")
                                    end
                                end
                            end
                        end
                    end
                end
            end
        elseif state.ranked and is_replay_op(message.op_code) then
            -- Ranked: kept for moderators (nobody watches these sessions).
            keep_stream(state, message)
        else
            -- Signaling, replay, and other match messages are forwarded unchanged.
            relay(dispatcher, state, message)
        end
    end
    -- Matches are not stopped automatically when empty (or never joined).
    if player_count(state) == 0 then
        state.empty_ticks = state.empty_ticks + 1
        if state.empty_ticks > 10 * 60 then
            finish(state)
            return nil
        end
    else
        state.empty_ticks = 0
    end
    return state
end

local function match_terminate(context, dispatcher, tick, state, grace_seconds)
    finish(state)
    return nil
end

-- ikemen_rating.lua signals {kind = "ranked_claim", set} when both players
-- have claimed the result of a set (it is settled or disputed): the set is
-- decided.
local function match_signal(context, dispatcher, tick, state, data)
    local ok, sig = pcall(nk.json_decode, data or "")
    if ok and type(sig) == "table" and sig.kind == "ranked_claim" and tonumber(sig.set) == state.set_no then
        set_decided(state)
    end
    return state, data
end

return {
    match_init = match_init,
    match_join_attempt = match_join_attempt,
    match_join = match_join,
    match_leave = match_leave,
    match_loop = match_loop,
    match_terminate = match_terminate,
    match_signal = match_signal,
}
