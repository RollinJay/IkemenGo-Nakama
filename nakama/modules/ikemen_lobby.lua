local nk = require("nakama")

-- IKEMEN GO authoritative lobby.
-- The lobby owns membership and 1v1 scheduling. It does not simulate gameplay.
-- Default format: FIFO queue. Optional winner_stays_on keeps the winner in
-- the active seat until max_games consecutive matches have been played.

local MAX_PLAYERS = 8
local OP_LOBBY_PAIRING = 0x494B4C10
local OP_LOBBY_RESULT = 0x494B4C11
local OP_LOBBY_STATE = 0x494B4C12

local function player_count(state)
    local count = 0
    for _ in pairs(state.players) do
        count = count + 1
    end
    return count
end

local function normalize_format(format)
    if format == nil or format == "" then
        return "queue"
    end
    if format == "winner_stays_on" or format == "winner_stays" then
        return "winner_stays_on"
    end
    if format == "queue" or format == "round_robin" or format == "bracket" then
        return format
    end
    return "queue"
end

local function normalize_max_games(value)
    local n = tonumber(value)
    if n == nil or n ~= math.floor(n) or n < 1 then
        return 3
    end
    if n > 99 then
        return 99
    end
    return n
end

local function queue_match(state)
    local queue = state.tournament.queue
    while #queue > 0 and not state.players[queue[1]] do
        table.remove(queue, 1)
    end
    if #queue < 2 then
        state.current_match = nil
        return false
    end
    local player1 = table.remove(queue, 1)
    local player2 = table.remove(queue, 1)
    state.current_match = { player1 = player1, player2 = player2 }
    return true
end

local function queue_append(state, user_id)
    if user_id ~= nil and state.players[user_id] then
        state.tournament.queue[#state.tournament.queue + 1] = user_id
    end
end

local function ordered_players(state)
    local ids = {}
    for i, user_id in ipairs(state.player_order) do
        if state.players[user_id] then
            ids[#ids + 1] = user_id
        end
    end
    return ids
end

local function current_pair_payload(state)
    if not state.current_match then
        return nil
    end
    return {
        kind = "lobby_pairing",
        format = state.format,
        player1 = state.current_match.player1,
        player2 = state.current_match.player2,
        round = state.tournament.round,
        match_index = state.tournament.match_index,
        queue = state.tournament.queue,
        max_games = state.max_games,
        winner_streak = state.tournament.winner_streak,
    }
end

local function broadcast_pairing(dispatcher, state)
    local payload = current_pair_payload(state)
    if payload ~= nil then
        dispatcher.broadcast_message(
            OP_LOBBY_PAIRING,
            nk.json_encode(payload),
            nil,
            nil
        )
    end
end

local function broadcast_state(dispatcher, state)
    dispatcher.broadcast_message(
        OP_LOBBY_STATE,
        nk.json_encode({
            player_count = player_count(state),
            players = ordered_players(state),
            format = state.format,
            max_games = state.max_games,
            queue = state.tournament.queue,
            tournament = state.tournament,
            current_match = state.current_match,
        }),
        nil,
        nil
    )
end

local function make_round_robin(ids)
    local schedule = {}
    for i = 1, #ids do
        for j = i + 1, #ids do
            schedule[#schedule + 1] = { player1 = ids[i], player2 = ids[j] }
        end
    end
    return schedule
end

local function make_bracket_round(ids)
    local round = {}
    local i = 1
    while i <= #ids do
        round[#round + 1] = {
            player1 = ids[i],
            player2 = ids[i + 1],
            winner = nil,
        }
        i = i + 2
    end
    return round
end

local function start_next_bracket_round(state)
    local winners = {}
    for _, match in ipairs(state.tournament.bracket_round) do
        local winner = match.winner
        if winner == nil and match.player2 == nil then
            winner = match.player1
        end
        if winner ~= nil then
            winners[#winners + 1] = winner
        end
    end

    if #winners <= 1 then
        state.current_match = nil
        state.tournament.finished = true
        state.tournament.winner = winners[1]
        return
    end

    state.tournament.round = state.tournament.round + 1
    state.tournament.match_index = 1
    state.tournament.bracket_round = make_bracket_round(winners)
    state.current_match = nil
end

local function activate_bracket_match(state)
    while not state.tournament.finished do
        local match = state.tournament.bracket_round[state.tournament.match_index]
        if match == nil then
            start_next_bracket_round(state)
            if state.tournament.finished then
                return
            end
            match = state.tournament.bracket_round[state.tournament.match_index]
            if match == nil then
                state.current_match = nil
                state.tournament.finished = true
                return
            end
        end

        if match.player2 == nil then
            match.winner = match.player1
            state.tournament.match_index = state.tournament.match_index + 1
        else
            state.current_match = {
                player1 = match.player1,
                player2 = match.player2,
            }
            return
        end
    end
    state.current_match = nil
end

local function advance_lobby_result(state, winner_id, loser_id)
    if state.current_match == nil then
        return false, "There is no active lobby match"
    end
    local current = state.current_match
    local valid_pair = (current.player1 == winner_id and current.player2 == loser_id) or
        (current.player2 == winner_id and current.player1 == loser_id)
    if not valid_pair then
        return false, "Result does not match the active pairing"
    end

    state.last_result = {
        winner = winner_id,
        loser = loser_id,
    }

    if state.format == "queue" then
        -- FIFO lobby: both players have completed their turn and go to the
        -- end of the line. The next two waiting players take the match.
        queue_append(state, current.player1)
        queue_append(state, current.player2)
        state.tournament.winner_streak = 0
        queue_match(state)
        return true
    end

    if state.format == "winner_stays_on" then
        local streak = state.tournament.winner_streak or 0
        if state.tournament.streak_player == winner_id then
            streak = streak + 1
        else
            state.tournament.streak_player = winner_id
            streak = 1
        end

        -- The loser always goes to the end of the waiting line. If the winner
        -- has reached the lobby's consecutive-game cap, the winner also goes
        -- to the end and the next waiting pair takes the match.
        queue_append(state, loser_id)
        if streak >= state.max_games then
            queue_append(state, winner_id)
            state.tournament.winner_streak = 0
            state.tournament.streak_player = nil
            queue_match(state)
        else
            state.tournament.winner_streak = streak
            local next_opponent = table.remove(state.tournament.queue, 1)
            if next_opponent ~= nil and state.players[next_opponent] then
                state.current_match = { player1 = winner_id, player2 = next_opponent }
            else
                -- With only two players the loser is returned to the queue, so
                -- the winner can continue against that player.
                queue_match(state)
            end
        end
        return true
    end

    if state.format == "round_robin" then
        state.tournament.schedule_index = state.tournament.schedule_index + 1
        local next_match = state.tournament.schedule[state.tournament.schedule_index]
        if next_match == nil then
            state.current_match = nil
            state.tournament.finished = true
        else
            state.current_match = {
                player1 = next_match.player1,
                player2 = next_match.player2,
            }
        end
        return true
    end

    local bracket_match = state.tournament.bracket_round[state.tournament.match_index]
    if bracket_match == nil then
        return false, "Tournament bracket state is invalid"
    end
    bracket_match.winner = winner_id
    state.tournament.match_index = state.tournament.match_index + 1

    activate_bracket_match(state)
    return true
end

local function initialize_schedule(state)
    local ids = ordered_players(state)
    if #ids < 2 or state.tournament.started then
        return
    end

    state.tournament.started = true
    state.tournament.finished = false
    state.tournament.round = 1
    state.tournament.match_index = 1

    if state.format == "round_robin" then
        state.tournament.schedule = make_round_robin(ids)
        local first = state.tournament.schedule[1]
        state.current_match = {
            player1 = first.player1,
            player2 = first.player2,
        }
        return
    end

    if state.format == "bracket" then
        state.tournament.bracket_round = make_bracket_round(ids)
        activate_bracket_match(state)
        return
    end

    -- Queue-based lobbies are the default. The first two players take the
    -- first turn; every completed turn moves both players to the back.
    state.tournament.queue = {}
    for i = 1, #ids do
        state.tournament.queue[#state.tournament.queue + 1] = ids[i]
    end
    state.tournament.winner_streak = 0
    state.tournament.streak_player = nil
    queue_match(state)
end

local function match_init(context, params)
    params = params or {}
    local format = normalize_format(params.format)
    local max_games = normalize_max_games(params.max_games)
    if params.rules and params.rules.max_games ~= nil then
        max_games = normalize_max_games(params.rules.max_games)
    end
    local state = {
        creator = params.creator or context.user_id,
        game = params.game or "",
        build = params.build or "",
        name = params.name or "IKEMEN Lobby",
        format = format,
        max_games = max_games,
        rules = params.rules or {},
        players = {},
        player_order = {},
        tournament = {
            started = false,
            finished = false,
            round = 0,
            match_index = 0,
            schedule = {},
            schedule_index = 1,
            queue = {},
            bracket_round = {},
            winner_streak = 0,
            streak_player = nil,
        },
        current_match = nil,
        last_result = nil,
    }
    return state, 10, nk.json_encode({
        kind = "ikemen-lobby",
        game = state.game,
        build = state.build,
        name = state.name,
        format = state.format,
        max_players = MAX_PLAYERS,
        max_games = state.max_games,
    })
end

local function match_join_attempt(context, dispatcher, tick, state, presence, metadata)
    if state.tournament.started and state.format ~= "queue" and state.format ~= "winner_stays_on" then
        return state, false, "Lobby tournament has already started"
    end
    if player_count(state) >= MAX_PLAYERS then
        return state, false, "Lobby is full"
    end
    metadata = metadata or {}
    if state.game ~= "" and tostring(metadata.game or "") ~= state.game then
        return state, false, "Lobby game identity does not match"
    end
    if state.build ~= "" and tostring(metadata.build or "") ~= state.build then
        return state, false, "Lobby build identity does not match"
    end
    return state, true
end

local function match_join(context, dispatcher, tick, state, presences)
    for _, presence in ipairs(presences) do
        if not state.players[presence.user_id] then
            state.players[presence.user_id] = {
                session_id = presence.session_id,
                username = presence.username,
            }
            state.player_order[#state.player_order + 1] = presence.user_id
            if state.tournament.started and
                (state.format == "queue" or state.format == "winner_stays_on") then
                queue_append(state, presence.user_id)
            end
        end
    end
    initialize_schedule(state)
    broadcast_state(dispatcher, state)
    broadcast_pairing(dispatcher, state)
    return state
end

local function match_leave(context, dispatcher, tick, state, presences)
    for _, presence in ipairs(presences) do
        state.players[presence.user_id] = nil
    end
    if player_count(state) == 0 then
        return nil
    end

    -- A departure invalidates the current scheduler rather than silently
    -- producing a pairing with a missing player. The lobby can be restarted
    -- by the remaining game UI once membership is stable.
    if state.current_match and
        (not state.players[state.current_match.player1] or
         (state.current_match.player2 ~= nil and not state.players[state.current_match.player2])) then
        state.current_match = nil
        state.tournament.started = false
        state.tournament.finished = false
        state.tournament.queue = {}
        state.tournament.winner_streak = 0
        state.tournament.streak_player = nil
        initialize_schedule(state)
    end
    broadcast_state(dispatcher, state)
    return state
end

local function match_loop(context, dispatcher, tick, state, messages)
    for _, message in ipairs(messages) do
        if message.op_code == OP_LOBBY_RESULT then
            local sender_id = message.sender and message.sender.user_id or ""
            local ok, command = pcall(nk.json_decode, message.data or "{}")
            if ok and type(command) == "table" and command.kind == "match_result" then
                if state.current_match ~= nil and
                    (sender_id == state.current_match.player1 or sender_id == state.current_match.player2) then
                    local result_ok, reason = advance_lobby_result(
                        state,
                        tostring(command.winner or ""),
                        tostring(command.loser or "")
                    )
                    if result_ok then
                        broadcast_state(dispatcher, state)
                        broadcast_pairing(dispatcher, state)
                    else
                        dispatcher.broadcast_message(
                            OP_LOBBY_STATE,
                            nk.json_encode({ kind = "lobby_error", error = reason or "invalid result" }),
                            nil,
                            nil
                        )
                    end
                end
            end
        else
            -- Signaling and replay messages are forwarded unchanged.
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

local function create_lobby(context, payload)
    local params = nk.json_decode(payload or "{}")
    params = params or {}
    params.kind = "ikemen-lobby"
    params.creator = context.user_id
    params.max_players = MAX_PLAYERS
    params.format = normalize_format(params.format)
    params.max_games = normalize_max_games(params.max_games)
    if params.rules == nil then
        params.rules = {}
    end
    if params.rules.max_games == nil then
        params.rules.max_games = params.max_games
    else
        params.max_games = normalize_max_games(params.rules.max_games)
    end

    local match_id = nk.match_create("ikemen_lobby", params)
    return nk.json_encode({ match_id = match_id })
end

nk.register_rpc(create_lobby, "ikemen_lobby_create")

return {
    match_init = match_init,
    match_join_attempt = match_join_attempt,
    match_join = match_join,
    match_leave = match_leave,
    match_loop = match_loop,
    match_terminate = match_terminate,
    match_signal = match_signal,
}
