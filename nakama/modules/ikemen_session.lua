local nk = require("nakama")

-- IKEMEN GO matchmaking/session coordination match.
-- This match is a Nakama relay for P2P signaling and delayed replay chunks.
-- It does not carry the GGPO gameplay stream.

local MAX_PLAYERS = 8
local OP_REMATCH_CHOICE = 1229673027
local OP_REMATCH_DECISION = 1229673028

local function rematch_decision(dispatcher, state, action, snapshot, source_user_id, reason)
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
    }
    for _, entry in ipairs(params.expected_users or {}) do
        if entry.presence and entry.presence.user_id then
            state.expected[entry.presence.user_id] = true
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
        }
    end
    return state
end

local function match_leave(context, dispatcher, tick, state, presences)
    local leaving_id = nil
    for _, presence in ipairs(presences) do
        if leaving_id == nil then
            leaving_id = presence.user_id
        end
        state.players[presence.user_id] = nil
        state.rematch_choices[presence.user_id] = nil
    end
    local count = 0
    for _ in pairs(state.players) do
        count = count + 1
    end
    if count == 0 then
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
                        if action == "exit" then
                            rematch_decision(dispatcher, state, "exit", nil, sender_id, "player_exit")
                        elseif action == "forfeit" then
                            if state.mode == "ranked" and not state.rematch_set_final then
                                rematch_decision(dispatcher, state, "forfeit", nil, sender_id, "player_forfeit")
                            elseif state.rematch_set_final then
                                rematch_decision(dispatcher, state, "exit", nil, sender_id, "invalid_final_action")
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
        else
            -- Signaling, replay, and other match messages are forwarded unchanged.
            dispatcher.broadcast_message(message.op_code, message.data, nil, nil)
        end
    end
    return state
end

local function match_terminate(context, dispatcher, tick, state, grace_seconds)
    return nil
end

local function match_signal(context, dispatcher, tick, state, data)
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
