local nk = require("nakama")

-- IKEMEN GO relay match, for the two players of a P2P session.
--
-- A P2P session whose UDP paths do not work falls back to a relay through the
-- server (src/nakama_p2ppath.go): each game sends its datagrams over its
-- realtime socket, and the server passes them on to the other game. This
-- match is that relay. The session or lobby that paired the two players makes
-- it when one of them asks for it (the ikemen_relay RPC below), for those two
-- alone:
--
--   * it admits the two players only, each with one presence;
--   * it passes relay data (OP_RELAY) from each player to the other only,
--     within a limit on the size of a datagram and on how many datagrams and
--     bytes each player sends, and drops everything else;
--   * it ends when a player leaves, and after IDLE_SECONDS without data.
--
-- The datagrams are sealed between the two games (p2pCrypt): the relay passes
-- on what it cannot read, and could not change unnoticed. On a server without
-- this module, the games fall back to a Nakama relayed match, which any
-- signed-in player who learns its id can join and flood.

-- The client's nakamaRelayOp (src/nakama_client.go).
local OP_RELAY = 0x494B524C
-- The relay passes on what arrived by each tick, so its tick rate bounds the
-- delay it adds: at Nakama's highest, 60, at most 17 ms (8 on average).
local TICK_RATE = 60
-- The largest datagram passed on (bytes). A game's largest is a segment of the
-- netplay stream (KCP, 1200 bytes) with its headers and the seal: under 1300.
local DATAGRAM_MAX = 2048
-- What each player may send: MESSAGE_RATE datagrams and BYTE_RATE bytes a
-- second, and at most MESSAGE_BURST datagrams and BYTE_BURST bytes at once.
-- A fight sends about 125 datagrams a second each way (GGPO's input and its
-- ack each frame, its reports, the mux's pings) and a few kilobytes. What goes
-- over is dropped, as a busy network drops it; GGPO and KCP send again what
-- they need. The bursts also keep what one tick passes on to a player well
-- within Nakama's outgoing queue (socket.outgoing_queue_size, 64 messages by
-- default), which closes the session of a player it overflows.
local MESSAGE_RATE = 300
local MESSAGE_BURST = 16
local BYTE_RATE = 64 * 1024
local BYTE_BURST = 32 * 1024
-- The relay ends after this long without data from either player (seconds):
-- a session on it pings every second, and one that moved to a UDP path leaves
-- the relay quiet.
local IDLE_SECONDS = 30
-- The kind of the ikemen_relay RPC's signal to a session or lobby.
local SIGNAL_KIND = "ikemen-relay"
-- The longest match or user id the RPC takes: Nakama's are under 64 bytes.
local ID_MAX = 128

local function pair_key(a, b)
    if a < b then
        return a .. "|" .. b
    end
    return b .. "|" .. a
end

-- The relay of players a and b, made by the session or lobby (parent_id, with
-- the given state) that paired them: the one it made for them before, while
-- that runs, or a new one.
local function relay_of(state, parent_id, a, b)
    state.p2p_relays = state.p2p_relays or {}
    local key = pair_key(a, b)
    local id = state.p2p_relays[key]
    if id ~= nil then
        local ok, match = pcall(nk.match_get, id)
        if ok and match ~= nil then
            return id
        end
    end
    id = nk.match_create("ikemen_relay", { player1 = a, player2 = b, parent = parent_id })
    state.p2p_relays[key] = id
    return id
end

-- The answer of a session or lobby to the ikemen_relay RPC's signal (data,
-- {kind, user_id, peer_id}): the relay of the two players when
-- is_pair(user_id, peer_id) says the match paired them, a refusal otherwise.
-- nil when data is not that signal.
local function answer(context, state, data, is_pair)
    local ok, signal = pcall(nk.json_decode, data or "")
    if not ok or type(signal) ~= "table" or signal.kind ~= SIGNAL_KIND then
        return nil
    end
    local user_id, peer_id = tostring(signal.user_id or ""), tostring(signal.peer_id or "")
    if user_id == "" or peer_id == "" or user_id == peer_id or not is_pair(user_id, peer_id) then
        return nk.json_encode({ error = "not_paired" })
    end
    local made, id = pcall(relay_of, state, context.match_id, user_id, peer_id)
    if not made then
        nk.logger_error(string.format("ikemen relay: no relay for %s and %s in %s: %s", user_id, peer_id,
            tostring(context.match_id), tostring(id)))
        return nk.json_encode({ error = "failed" })
    end
    return nk.json_encode({ relay_id = id })
end

local function match_init(context, params)
    params = params or {}
    local a, b = tostring(params.player1 or ""), tostring(params.player2 or "")
    if a == "" or b == "" or a == b then
        error("a relay is for two players")
    end
    local state = {
        parent = tostring(params.parent or ""),
        -- Each player's other player, and their presences once joined.
        peer = { [a] = b, [b] = a },
        presences = {},
        -- What each player may still send (MESSAGE_RATE, BYTE_RATE), and
        -- whether the log says they sent more.
        allowance = {
            [a] = { messages = MESSAGE_BURST, bytes = BYTE_BURST },
            [b] = { messages = MESSAGE_BURST, bytes = BYTE_BURST },
        },
        over = {},
        -- The tick of the last data from a player.
        last_data = 0,
    }
    return state, TICK_RATE, nk.json_encode({ kind = "ikemen-relay" })
end

local function match_join_attempt(context, dispatcher, tick, state, presence, metadata)
    local user_id = presence.user_id
    if state.peer[user_id] == nil then
        return state, false, "This relay is for two other players"
    end
    if state.presences[user_id] ~= nil then
        return state, false, "This player is in the relay already"
    end
    return state, true
end

local function match_join(context, dispatcher, tick, state, presences)
    for _, presence in ipairs(presences) do
        if state.peer[presence.user_id] ~= nil then
            state.presences[presence.user_id] = presence
        end
    end
    return state
end

-- A player who leaves ends the relay: the session over it is over. A game
-- leaves the relay when its session ends, and the server leaves it for a game
-- whose connection ended.
local function match_leave(context, dispatcher, tick, state, presences)
    for _, presence in ipairs(presences) do
        local joined = state.presences[presence.user_id]
        if joined ~= nil and joined.session_id == presence.session_id then
            return nil
        end
    end
    return state
end

local function note_over(context, state, user_id)
    if state.over[user_id] then
        return
    end
    state.over[user_id] = true
    nk.logger_warn(string.format("ikemen relay %s (of %s): %s sends more than the relay passes on; the rest is dropped",
        tostring(context.match_id), state.parent, user_id))
end

local function match_loop(context, dispatcher, tick, state, messages)
    for _, allowance in pairs(state.allowance) do
        allowance.messages = math.min(MESSAGE_BURST, allowance.messages + MESSAGE_RATE / TICK_RATE)
        allowance.bytes = math.min(BYTE_BURST, allowance.bytes + BYTE_RATE / TICK_RATE)
    end
    for _, message in ipairs(messages) do
        local sender = message.sender
        local user_id = sender and sender.user_id or ""
        local allowance = state.allowance[user_id]
        local data = message.data or ""
        local size = #data
        if allowance ~= nil and message.op_code == OP_RELAY and size > 0 then
            state.last_data = tick
            if size > DATAGRAM_MAX or allowance.messages < 1 or allowance.bytes < size then
                note_over(context, state, user_id)
            else
                allowance.messages = allowance.messages - 1
                allowance.bytes = allowance.bytes - size
                local to = state.presences[state.peer[user_id]]
                if to ~= nil then
                    dispatcher.broadcast_message(OP_RELAY, data, { to }, sender, false)
                end
            end
        end
    end
    if tick - state.last_data > IDLE_SECONDS * TICK_RATE then
        return nil
    end
    return state
end

local function match_terminate(context, dispatcher, tick, state, grace_seconds)
    return nil
end

-- Nothing signals a relay: the ikemen_relay RPC signals sessions and lobbies.
local function match_signal(context, dispatcher, tick, state, data)
    return state, nil
end

-- ikemen_relay: {match_id, peer_id} -> {match_id}. The relay of the caller and
-- peer_id, whom the session or lobby match_id paired: the session or lobby
-- answers the RPC's signal (see answer above) with the relay it made for the
-- two before, while that runs, or a new one. Errors: 3 without both ids, 9
-- when match_id is not a running session or lobby that paired the two, 13 when
-- the relay could not be made, and 16 without a player's session. Never 5: a
-- game takes "not found" for a server without this module, and falls back to
-- a Nakama relayed match.
local function rpc_relay(context, payload)
    if context.user_id == nil or context.user_id == "" then
        error({ "the relay is for players: call this RPC with a player's session", 16 })
    end
    local ok, request = pcall(nk.json_decode, payload or "")
    if not ok or type(request) ~= "table" then
        error({ "match_id and peer_id are required", 3 })
    end
    for _, field in ipairs({ "match_id", "peer_id" }) do
        local value = request[field]
        if type(value) ~= "string" or value == "" or #value > ID_MAX then
            error({ "match_id and peer_id are required", 3 })
        end
    end
    local signalled, reply = pcall(nk.match_signal, request.match_id,
        nk.json_encode({ kind = SIGNAL_KIND, user_id = context.user_id, peer_id = request.peer_id }))
    local decoded, result = false, nil
    if signalled then
        decoded, result = pcall(nk.json_decode, reply or "")
    end
    if not decoded or type(result) ~= "table" then
        error({ "match_id is not a session or lobby that is running", 9 })
    end
    if type(result.relay_id) == "string" and result.relay_id ~= "" then
        return nk.json_encode({ match_id = result.relay_id })
    end
    if result.error == "failed" then
        error({ "the relay could not be made", 13 })
    end
    error({ "the session or lobby did not pair you with peer_id", 9 })
end

nk.register_rpc(rpc_relay, "ikemen_relay")

return {
    -- For the session and lobby modules.
    answer = answer,
    -- The relay's match handler (nk.match_create("ikemen_relay", ...)).
    match_init = match_init,
    match_join_attempt = match_join_attempt,
    match_join = match_join,
    match_leave = match_leave,
    match_loop = match_loop,
    match_terminate = match_terminate,
    match_signal = match_signal,
}
