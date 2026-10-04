local nk = require("nakama")

-- IKEMEN GO authoritative lobby.
--
-- The lobby owns membership, settings, ready states and 1v1 scheduling. It
-- does not simulate gameplay: the two players of a pairing open a P2P
-- netplay session between themselves and report the result here.
--
-- Phases
--   waiting   no match is running; members ready up
--   playing   the current pairing is playing
--   results   a match has ended; the next pairing starts when the
--             lobby's interval has passed
--   finished  a round-robin or bracket tournament is over
--
-- Formats
--   queue            FIFO rotation: both players go to the back after a game
--   winner_stays_on  the winner stays on for up to max_games straight wins
--   round_robin      every entrant plays every other entrant once
--   bracket          single elimination
--
-- Only ready members are paired. In the queue formats a member who is not
-- ready keeps their place in line and is skipped. A tournament starts when
-- the host starts it (start = "host") or when every member is ready
-- (start = "auto"); its entrants are the members who are ready at that
-- moment, and each of its matches waits until both entrants are ready.
--
-- Watching: the pairing's player 1 (the netplay host) publishes the match's
-- replay stream (the rollback-resolved inputs, see src/replay_stream.go) when
-- the lobby allows spectators (settings.watch > 0, the publication delay in
-- seconds). The lobby relays it to the members who are not playing and keeps
-- it until the next match, so that a member who starts watching late (the
-- "watch" command) gets it from the beginning. A pairing whose player is
-- still watching the previous match waits for them.

local MAX_PLAYERS = 8
local TICK_RATE = 10

local OP_LOBBY_PAIRING = 0x494B4C10
local OP_LOBBY_RESULT = 0x494B4C11
local OP_LOBBY_STATE = 0x494B4C12
local OP_LOBBY_COMMAND = 0x494B4C13
local OP_LOBBY_EVENT = 0x494B4C14
local OP_P2P_SIGNAL = 0x494B5347
local OP_REPLAY_HEADER = 0x494B5210
local OP_REPLAY_CHUNK = 0x494B5211
local OP_REPLAY_END = 0x494B5212
local OP_REPLAY_RESET = 0x494B5213

-- Stored replay stream messages: for each stream of the match (a Turns match
-- publishes one per character change), the header, one chunk per 30 frames
-- and the end. A 20-minute match (MATCH_TIMEOUT_TICKS) needs about 2400.
local STREAM_MESSAGE_LIMIT = 2500
-- Stored messages sent per tick to a member catching up. Nakama closes a
-- session whose outgoing queue overflows (socket.outgoing_queue_size, 64 by
-- default); 8 per tick is 80 chunks, 40 seconds of play, per second.
local BACKFILL_PER_TICK = 8
-- Largest stream messages accepted (bytes): a header carries the sync
-- settings (about 6 KB), a chunk 30 frames (under 3 KB). The stored stream
-- is also capped in bytes, so that no player can make a lobby hold more.
local STREAM_HEADER_MAX = 16384
local STREAM_CHUNK_MAX = 8192
local STREAM_BYTES_LIMIT = 8 * 1024 * 1024
-- Longest spectator delay (seconds). A stream's end sends the last delay at
-- once (two chunks per second), and a spectator waiting for a Turns match's
-- next character waits the delay too; both stay well within limits at 10.
local WATCH_DELAY_MAX = 10

-- Connection types a member may report (the empty string is unknown).
local CONNECTIONS = { "wired", "wifi", "mobile" }
local LINK_LIMIT_MS = 9999

local CODE_COLLECTION = "ikemen_lobby_codes"
local CODE_ALPHABET = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
local CODE_LENGTH = 6

local NAME_LIMIT = 24
local COMMENT_LIMIT = 40
local CHAT_LIMIT = 80
local CHAT_INTERVAL_TICKS = 5
local EMPTY_TIMEOUT_TICKS = 60 * TICK_RATE
local MATCH_TIMEOUT_TICKS = 20 * 60 * TICK_RATE
local MAX_ATTEMPTS = 2

local DEFAULT_SETTINGS = {
    name = "IKEMEN Lobby",
    comment = "",
    size = MAX_PLAYERS,
    format = "queue",
    max_games = 3,
    rounds = 0,
    time = 0,
    teams = "any",
    stage = "select",
    start = "auto",
    interval = 5,
    private = false,
    watch = 3,
}

-- ---------------------------------------------------------------------------
-- Settings

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

local function is_queue_format(format)
    return format == "queue" or format == "winner_stays_on"
end

-- Removes control characters and cuts the text to at most limit characters
-- without splitting a UTF-8 sequence.
local function clean_text(value, limit)
    if value == nil then
        return nil
    end
    local text = tostring(value)
    local out = {}
    local count = 0
    local i = 1
    local len = #text
    while i <= len and count < limit do
        local c = text:byte(i)
        local size = 1
        if c >= 0xF0 then
            size = 4
        elseif c >= 0xE0 then
            size = 3
        elseif c >= 0xC0 then
            size = 2
        end
        if c >= 0x20 and c ~= 0x7F and (c < 0x80 or c >= 0xC0) then
            out[#out + 1] = text:sub(i, i + size - 1)
            count = count + 1
        end
        i = i + size
    end
    local result = table.concat(out)
    result = result:gsub("^%s+", ""):gsub("%s+$", "")
    return result
end

local function int_in(value, fallback, low, high)
    local n = tonumber(value)
    if n == nil then
        return fallback
    end
    n = math.floor(n)
    if n < low then
        return low
    end
    if n > high then
        return high
    end
    return n
end

local function one_of(value, fallback, allowed)
    for _, v in ipairs(allowed) do
        if value == v then
            return value
        end
    end
    return fallback
end

-- Returns a complete, valid settings table: input fields override base,
-- missing or invalid fields keep base's value.
local function normalize_settings(input, base)
    input = type(input) == "table" and input or {}
    base = base or DEFAULT_SETTINGS
    local s = {}
    local name = clean_text(input.name, NAME_LIMIT)
    s.name = (name ~= nil and name ~= "") and name or base.name
    local comment = clean_text(input.comment, COMMENT_LIMIT)
    if comment == nil then
        comment = base.comment
    end
    s.comment = comment
    s.size = int_in(input.size, base.size, 2, MAX_PLAYERS)
    s.format = input.format ~= nil and normalize_format(input.format) or base.format
    s.max_games = input.max_games ~= nil and normalize_max_games(input.max_games) or base.max_games
    s.rounds = int_in(input.rounds, base.rounds, 0, 9)
    local time = tonumber(input.time)
    if time == nil then
        s.time = base.time
    elseif time < 0 then
        s.time = -1
    elseif time == 0 then
        s.time = 0
    else
        s.time = int_in(time, base.time, 10, 999)
    end
    s.teams = one_of(input.teams, base.teams, { "any", "single" })
    s.stage = one_of(input.stage, base.stage, { "select", "random" })
    s.start = one_of(input.start, base.start, { "auto", "host" })
    s.interval = int_in(input.interval, base.interval, 3, 30)
    if input.private == nil then
        s.private = base.private
    else
        s.private = input.private == true or input.private == "true" or input.private == 1
    end
    -- Seconds the watched match runs behind the players; 0 turns watching off.
    s.watch = int_in(input.watch, base.watch or DEFAULT_SETTINGS.watch, 0, WATCH_DELAY_MAX)
    return s
end

local function normalize_code(code)
    if code == nil then
        return ""
    end
    local cleaned = tostring(code):upper():gsub("[^%w]", "")
    return cleaned
end

-- ---------------------------------------------------------------------------
-- Membership helpers

local function player_count(state)
    local count = 0
    for _ in pairs(state.players) do
        count = count + 1
    end
    return count
end

local function ordered_players(state)
    local ids = {}
    for _, user_id in ipairs(state.player_order) do
        if state.players[user_id] then
            ids[#ids + 1] = user_id
        end
    end
    return ids
end

local function is_ready(state, user_id)
    local p = user_id and state.players[user_id]
    return p ~= nil and p.ready == true
end

local function is_watching(state, user_id)
    local p = user_id and state.players[user_id]
    return p ~= nil and p.watching == true
end

local function record_for(state, user_id)
    local r = state.records[user_id]
    if r == nil then
        r = { wins = 0, losses = 0 }
        state.records[user_id] = r
    end
    return r
end

-- A member's online name: the account's display name, or its username (cut
-- to 16 characters) when the player has not chosen a name. Kept for members
-- who left (results).
local function name_of(state, user_id)
    local p = user_id and state.players[user_id]
    if p ~= nil then
        return p.name
    end
    return state.names[user_id] or ""
end

-- A member's account username, also kept for members who left.
local function username_of(state, user_id)
    local p = user_id and state.players[user_id]
    if p ~= nil then
        return p.username
    end
    return state.usernames[user_id] or ""
end

-- A member's tag: four digits from the account ID (its first eight hex digits
-- as a number, modulo 10000). Names need not be unique; the tag tells members
-- with the same name apart. Digits only, so that any font can draw a tag and
-- no tag mixes up look-alike characters such as 8 and B.
local function tag_of(user_id)
    local n = tonumber((tostring(user_id or ""):gsub("-", ""):sub(1, 8)), 16)
    if n == nil then
        return ""
    end
    return string.format("%04d", n % 10000)
end

-- The display name of an account, "" when it has none or cannot be read.
local function account_display_name(user_id)
    local ok, users = pcall(nk.users_get_id, { user_id })
    if ok and type(users) == "table" and type(users[1]) == "table" and type(users[1].display_name) == "string" then
        return users[1].display_name
    end
    return ""
end

local function pair_key(a, b)
    if a < b then
        return a .. "|" .. b
    end
    return b .. "|" .. a
end

local function pair_failed(state, a, b)
    return state.failed_pairs[pair_key(a, b)] == true
end

-- Round trip in ms between two members over their direct (P2P) connection,
-- as the last of them reported it, or nil when they have not measured one.
local function link_rtt(state, a, b)
    local link = a ~= nil and b ~= nil and state.links[pair_key(a, b)] or nil
    return link and link.rtt or nil
end

-- For each member, the members they have a measured direct connection with:
-- {user_id = rtt}.
local function links_of(state, user_id)
    local out = {}
    for _, link in pairs(state.links) do
        if link.a == user_id then
            out[link.b] = link.rtt
        elseif link.b == user_id then
            out[link.a] = link.rtt
        end
    end
    return out
end

local function forget_links(state, user_id)
    for key, link in pairs(state.links) do
        if link.a == user_id or link.b == user_id then
            state.links[key] = nil
        end
    end
end

-- ---------------------------------------------------------------------------
-- Queue formats

local function queue_remove(state, user_id)
    local queue = state.tournament.queue
    for i = #queue, 1, -1 do
        if queue[i] == user_id then
            table.remove(queue, i)
        end
    end
end

local function queue_append(state, user_id)
    if user_id ~= nil and state.players[user_id] then
        queue_remove(state, user_id)
        state.tournament.queue[#state.tournament.queue + 1] = user_id
    end
end

local function queue_prepend(state, user_id)
    if user_id ~= nil and state.players[user_id] then
        queue_remove(state, user_id)
        table.insert(state.tournament.queue, 1, user_id)
    end
end

local function queue_purge(state)
    local queue = state.tournament.queue
    for i = #queue, 1, -1 do
        if not state.players[queue[i]] then
            table.remove(queue, i)
        end
    end
end

-- First ready member in line that may play against partner (nil = anyone).
local function queue_first_ready(state, partner)
    for _, user_id in ipairs(state.tournament.queue) do
        if user_id ~= partner and is_ready(state, user_id) and
            (partner == nil or not pair_failed(state, partner, user_id)) then
            return user_id
        end
    end
    return nil
end

-- The next queue-format pairing, without changing the queue.
local function queue_next(state)
    queue_purge(state)
    local t = state.tournament
    if state.settings.format == "winner_stays_on" and t.streak_player ~= nil then
        if is_ready(state, t.streak_player) then
            local challenger = queue_first_ready(state, t.streak_player)
            if challenger ~= nil then
                return { player1 = t.streak_player, player2 = challenger }
            end
            return nil
        end
    end
    for i, first in ipairs(t.queue) do
        if is_ready(state, first) and
            not (state.settings.format == "winner_stays_on" and first == t.streak_player) then
            for j = i + 1, #t.queue do
                local second = t.queue[j]
                if is_ready(state, second) and not pair_failed(state, first, second) then
                    return { player1 = first, player2 = second }
                end
            end
        end
    end
    return nil
end

-- A champion who is not ready (or has left) gives up the seat.
local function queue_release_champion(state)
    local t = state.tournament
    if t.streak_player ~= nil and not is_ready(state, t.streak_player) then
        queue_append(state, t.streak_player)
        t.streak_player = nil
        t.winner_streak = 0
    end
end

-- ---------------------------------------------------------------------------
-- Tournaments

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
        state.tournament.finished = true
        state.tournament.winner = winners[1]
        return
    end

    state.tournament.round = state.tournament.round + 1
    state.tournament.match_index = 1
    state.tournament.bracket_round = make_bracket_round(winners)
end

-- Moves the bracket forward past byes and departed entrants and returns the
-- match that is due, or nil when the tournament is over.
local function bracket_due(state)
    local t = state.tournament
    while not t.finished do
        local match = t.bracket_round[t.match_index]
        if match == nil then
            start_next_bracket_round(state)
            if t.finished then
                return nil
            end
            match = t.bracket_round[t.match_index]
            if match == nil then
                t.finished = true
                return nil
            end
        end
        local p1_here = match.player1 ~= nil and state.players[match.player1] ~= nil
        local p2_here = match.player2 ~= nil and state.players[match.player2] ~= nil
        if p1_here and p2_here then
            return match
        end
        -- Bye, or a departed entrant: the present player (if any) advances.
        match.winner = (p1_here and match.player1) or (p2_here and match.player2) or nil
        t.match_index = t.match_index + 1
    end
    return nil
end

-- Ends a round robin. The entrant with the most wins takes it; when several
-- share the most wins, it has no single winner and t.tied lists them.
local function finish_round_robin(state)
    local t = state.tournament
    t.finished = true
    local best, leaders = -1, {}
    for _, user_id in ipairs(t.entrants or {}) do
        local points = (t.points or {})[user_id] or 0
        if points > best then
            best, leaders = points, { user_id }
        elseif points == best then
            leaders[#leaders + 1] = user_id
        end
    end
    if #leaders == 1 then
        t.winner = leaders[1]
        t.tied = nil
    else
        t.winner = nil
        t.tied = leaders
    end
end

-- Skips round-robin matches whose entrants have left and returns the match
-- that is due, or nil when the tournament is over.
local function round_robin_due(state)
    local t = state.tournament
    while true do
        local match = t.schedule[t.schedule_index]
        if match == nil then
            finish_round_robin(state)
            return nil
        end
        if state.players[match.player1] and state.players[match.player2] then
            return match
        end
        t.schedule_index = t.schedule_index + 1
    end
end

local function tournament_due(state)
    if not state.tournament.started or state.tournament.finished then
        return nil
    end
    if state.settings.format == "round_robin" then
        return round_robin_due(state)
    end
    return bracket_due(state)
end

-- Starts a tournament with the members who are ready now, in join order.
local function start_tournament(state)
    local entrants = {}
    for _, user_id in ipairs(ordered_players(state)) do
        if is_ready(state, user_id) then
            entrants[#entrants + 1] = user_id
        end
    end
    if #entrants < 2 then
        return false
    end
    local t = state.tournament
    t.started = true
    t.finished = false
    t.winner = nil
    t.tied = nil
    t.points = {}
    t.round = 1
    t.match_index = 1
    t.schedule_index = 1
    t.entrants = entrants
    t.attempts = 0
    if state.settings.format == "round_robin" then
        t.schedule = make_round_robin(entrants)
    else
        t.bracket_round = make_bracket_round(entrants)
    end
    return true
end

local function everyone_ready(state)
    local count = 0
    for user_id in pairs(state.players) do
        if not is_ready(state, user_id) then
            return false
        end
        count = count + 1
    end
    return count >= 2
end

-- ---------------------------------------------------------------------------
-- Scheduling

local function set_phase(state, phase, tick)
    state.phase = phase
    state.phase_tick = tick
    state.label_dirty = true
    if phase ~= "results" then
        state.results_until = nil
    end
end

local function lobby_running(state)
    if is_queue_format(state.settings.format) then
        return state.settings.start == "auto" or state.started
    end
    return state.tournament.started and not state.tournament.finished
end

-- The pairing that plays next, without starting it. For tournaments it may
-- name an entrant who is not ready yet; the match waits for them.
local function compute_next(state)
    if is_queue_format(state.settings.format) then
        if state.settings.format == "winner_stays_on" then
            queue_release_champion(state)
        end
        return queue_next(state)
    end
    local due = tournament_due(state)
    if due == nil then
        return nil
    end
    return { player1 = due.player1, player2 = due.player2 }
end

local function start_match(state, pair, tick)
    if is_queue_format(state.settings.format) then
        queue_remove(state, pair.player1)
        queue_remove(state, pair.player2)
    end
    state.current_match = { player1 = pair.player1, player2 = pair.player2 }
    state.match_seq = (state.match_seq or 0) + 1
    state.next_match = nil
    -- The previous match's stream and watchers are done with.
    state.stream = nil
    state.backfill = {}
    for _, p in pairs(state.players) do
        p.watching = false
    end
    set_phase(state, "playing", tick)
    state.pairing_dirty = true
end

-- Called after every change and on every tick. Starts the next match when
-- its players are ready, and ends the results phase.
local function schedule(state, tick)
    if state.phase == "playing" then
        return
    end
    if state.phase == "results" and state.results_until ~= nil and tick < state.results_until then
        return
    end
    local format = state.settings.format
    if not is_queue_format(format) then
        local t = state.tournament
        if t.started and not t.finished then
            compute_next(state)
        end
        if t.started and t.finished and state.phase ~= "finished" then
            -- The tournament has just ended. Members ready up again for the
            -- next one, so that it does not start by itself right away.
            set_phase(state, "finished", tick)
            state.next_match = nil
            for _, p in pairs(state.players) do
                p.ready = false
            end
            state.dirty = true
            return
        end
        if (not t.started or t.finished) and state.settings.start == "auto" and everyone_ready(state) then
            start_tournament(state)
            if state.phase == "finished" then
                set_phase(state, "waiting", tick)
            end
        end
        if t.started and t.finished then
            return
        end
    end
    local previous_phase = state.phase
    local previous_next = state.next_match
    local pair = nil
    if lobby_running(state) then
        pair = compute_next(state)
    end
    -- A player still watching the previous match (a spectator runs a few
    -- seconds behind) keeps the turn; the match starts when they are back.
    if pair ~= nil and is_ready(state, pair.player1) and is_ready(state, pair.player2) and
        not is_watching(state, pair.player1) and not is_watching(state, pair.player2) then
        start_match(state, pair, tick)
        state.dirty = true
        return
    end
    state.next_match = pair
    if previous_phase ~= "waiting" then
        set_phase(state, "waiting", tick)
    end
    if previous_phase ~= "waiting" or
        (previous_next == nil) ~= (pair == nil) or
        (pair ~= nil and (previous_next.player1 ~= pair.player1 or previous_next.player2 ~= pair.player2)) then
        state.dirty = true
    end
end

local function begin_results(state, tick, result)
    -- Names are kept with the result: a player may leave before it is shown.
    for _, field in ipairs({ "winner", "loser", "player1", "player2" }) do
        if result[field] ~= nil then
            result[field .. "_name"] = name_of(state, result[field])
        end
    end
    state.last_result = result
    state.current_match = nil
    set_phase(state, "results", tick)
    state.results_until = tick + state.settings.interval * TICK_RATE
    state.next_match = compute_next(state)
    state.dirty = true
    state.label_dirty = true
end

-- Applies a reported winner to the format's progress. Returns false and a
-- reason when the report does not match the current pairing.
local function advance_lobby_result(state, winner_id, loser_id, tick)
    local current = state.current_match
    if current == nil then
        return false, "There is no active lobby match"
    end
    local valid_pair = (current.player1 == winner_id and current.player2 == loser_id) or
        (current.player2 == winner_id and current.player1 == loser_id)
    if not valid_pair then
        return false, "Result does not match the active pairing"
    end

    local winner_record = record_for(state, winner_id)
    local loser_record = record_for(state, loser_id)
    winner_record.wins = winner_record.wins + 1
    loser_record.losses = loser_record.losses + 1

    local t = state.tournament
    local format = state.settings.format
    local streak = 0
    if format == "queue" then
        -- Both players have completed their turn and go to the back of the line.
        queue_append(state, current.player1)
        queue_append(state, current.player2)
        t.winner_streak = 0
    elseif format == "winner_stays_on" then
        streak = t.winner_streak or 0
        if t.streak_player == winner_id then
            streak = streak + 1
        else
            streak = 1
        end
        -- The loser goes to the back of the line. A winner who has reached
        -- max_games straight wins goes there too.
        queue_append(state, loser_id)
        if streak >= state.settings.max_games then
            queue_append(state, winner_id)
            t.streak_player = nil
            t.winner_streak = 0
        else
            queue_remove(state, winner_id)
            t.streak_player = winner_id
            t.winner_streak = streak
        end
    elseif format == "round_robin" then
        t.points = t.points or {}
        t.points[winner_id] = (t.points[winner_id] or 0) + 1
        t.schedule_index = t.schedule_index + 1
        t.attempts = 0
    else
        local match = t.bracket_round[t.match_index]
        if match == nil then
            return false, "Tournament bracket state is invalid"
        end
        match.winner = winner_id
        t.match_index = t.match_index + 1
        t.attempts = 0
    end

    begin_results(state, tick, {
        seq = state.match_seq,
        winner = winner_id,
        loser = loser_id,
        streak = streak,
    })
    return true
end

-- A match that produced no winner: a draw, an aborted session or players
-- who could not connect. connection is true when the players never got a
-- session going; that pair is not scheduled again in the queue formats.
local function no_contest(state, reason, connection, tick)
    local current = state.current_match
    if current == nil then
        return
    end
    local t = state.tournament
    local format = state.settings.format
    if reason == "aborted" then
        -- A player left the session (or it broke) mid-match: neither player
        -- is put straight into another match; they ready up again.
        for _, user_id in ipairs({ current.player1, current.player2 }) do
            if state.players[user_id] then
                state.players[user_id].ready = false
            end
        end
    end
    if is_queue_format(format) then
        if connection then
            state.failed_pairs[pair_key(current.player1, current.player2)] = true
            -- Neither player lost their turn: both return to the front.
            queue_prepend(state, current.player2)
            queue_prepend(state, current.player1)
            if t.streak_player == current.player1 or t.streak_player == current.player2 then
                -- The champion keeps the seat but cannot face this challenger.
                queue_remove(state, t.streak_player)
            end
        else
            queue_append(state, current.player1)
            queue_append(state, current.player2)
            if t.streak_player == current.player1 or t.streak_player == current.player2 then
                t.streak_player = nil
                t.winner_streak = 0
            end
        end
    elseif format == "round_robin" then
        t.attempts = (t.attempts or 0) + 1
        if t.attempts >= MAX_ATTEMPTS then
            t.schedule_index = t.schedule_index + 1
            t.attempts = 0
        end
    else
        t.attempts = (t.attempts or 0) + 1
        if t.attempts >= MAX_ATTEMPTS then
            -- The bracket needs a winner: the first-listed entrant advances.
            local match = t.bracket_round[t.match_index]
            if match ~= nil then
                match.winner = match.player1
                t.match_index = t.match_index + 1
            end
            t.attempts = 0
        end
    end
    begin_results(state, tick, {
        seq = state.match_seq,
        player1 = current.player1,
        player2 = current.player2,
        no_contest = true,
        reason = reason,
    })
end

-- ---------------------------------------------------------------------------
-- Broadcasting

-- Members who play next: the announced pairing, or during a queue-format
-- match the next ready players in line (in winner-stays only the next
-- challenger is known before the result).
local function upcoming(state)
    local set = {}
    local nxt = state.next_match
    if nxt ~= nil then
        set[nxt.player1] = true
        set[nxt.player2] = true
        return set
    end
    if state.phase == "playing" and is_queue_format(state.settings.format) then
        local need = state.settings.format == "queue" and 2 or 1
        for _, user_id in ipairs(state.tournament.queue) do
            if need == 0 then
                break
            end
            if is_ready(state, user_id) then
                set[user_id] = true
                need = need - 1
            end
        end
    end
    return set
end

local function member_state(state, user_id, next_set)
    local current = state.current_match
    if current ~= nil and (current.player1 == user_id or current.player2 == user_id) then
        return "playing"
    end
    if next_set[user_id] and is_ready(state, user_id) then
        return "next"
    end
    if state.players[user_id] ~= nil and state.players[user_id].watching then
        return "watching"
    end
    if is_ready(state, user_id) then
        return "ready"
    end
    return "standby"
end

local function member_list(state)
    local members = {}
    local next_set = upcoming(state)
    for _, user_id in ipairs(ordered_players(state)) do
        local p = state.players[user_id]
        local r = record_for(state, user_id)
        members[#members + 1] = {
            user_id = user_id,
            username = p.username,
            -- name: what lobbies show; display_name: the name the player chose
            -- ("" when none), which the fight screen shows.
            name = p.name,
            display_name = p.display_name,
            ready = p.ready == true,
            ping = p.ping,
            region = p.region,
            connection = p.connection or "",
            links = links_of(state, user_id),
            watching = p.watching == true,
            wins = r.wins,
            losses = r.losses,
            host = user_id == state.host,
            state = member_state(state, user_id, next_set),
        }
    end
    return members
end

local function current_pair_payload(state)
    if not state.current_match then
        return nil
    end
    return {
        kind = "lobby_pairing",
        format = state.settings.format,
        player1 = state.current_match.player1,
        player2 = state.current_match.player2,
        seq = state.match_seq or 0,
        round = state.tournament.round,
        match_index = state.tournament.match_index,
        queue = state.tournament.queue,
        max_games = state.settings.max_games,
        winner_streak = state.tournament.winner_streak,
        settings = state.settings,
    }
end

local function broadcast_pairing(dispatcher, state)
    local payload = current_pair_payload(state)
    if payload ~= nil then
        dispatcher.broadcast_message(OP_LOBBY_PAIRING, nk.json_encode(payload), nil, nil)
    end
end

local function state_payload(state, tick)
    local remaining = 0
    if state.phase == "results" and state.results_until ~= nil then
        remaining = math.max(0, (state.results_until - tick) / TICK_RATE)
    end
    local current = nil
    if state.current_match ~= nil then
        local p1, p2 = state.current_match.player1, state.current_match.player2
        current = {
            player1 = p1,
            player2 = p2,
            player1_name = name_of(state, p1),
            player2_name = name_of(state, p2),
            seq = state.match_seq,
            elapsed = math.floor((tick - (state.phase_tick or tick)) / TICK_RATE),
            -- The players' direct round trip, and whether the match's stream
            -- has started (members can watch it).
            link = link_rtt(state, p1, p2),
            watch = state.stream ~= nil and state.stream.seq == state.match_seq,
        }
    end
    local next_pair = nil
    if state.next_match ~= nil then
        next_pair = {
            player1 = state.next_match.player1,
            player2 = state.next_match.player2,
            player1_name = name_of(state, state.next_match.player1),
            player2_name = name_of(state, state.next_match.player2),
        }
    end
    local t = state.tournament
    local streak = nil
    if t.streak_player ~= nil and state.players[t.streak_player] then
        streak = { user_id = t.streak_player, count = t.winner_streak or 0 }
    end
    local tournament = nil
    if not is_queue_format(state.settings.format) then
        local tied_names = nil
        if t.tied ~= nil then
            tied_names = {}
            for _, user_id in ipairs(t.tied) do
                tied_names[#tied_names + 1] = name_of(state, user_id)
            end
        end
        tournament = {
            started = t.started,
            finished = t.finished,
            round = t.round,
            match_index = t.match_index,
            winner = t.winner,
            winner_name = t.winner and name_of(state, t.winner) or nil,
            -- Round robin: the entrants sharing the most wins when no one
            -- has more than the others.
            tied = t.tied,
            tied_names = tied_names,
            entrants = t.entrants or {},
        }
    end
    state.version = (state.version or 0) + 1
    return {
        kind = "lobby_state",
        version = state.version,
        code = state.code,
        host = state.host,
        phase = state.phase,
        phase_remaining = remaining,
        started = lobby_running(state) or state.phase == "finished",
        settings = state.settings,
        members = member_list(state),
        current = current,
        next = next_pair,
        queue = t.queue,
        streak = streak,
        last_result = state.last_result,
        tournament = tournament,
        -- Fields kept for clients written against the first lobby protocol.
        player_count = player_count(state),
        players = ordered_players(state),
        format = state.settings.format,
        max_games = state.settings.max_games,
        seq = state.match_seq or 0,
        current_match = state.current_match,
    }
end

local function broadcast_state(dispatcher, state, tick)
    dispatcher.broadcast_message(OP_LOBBY_STATE, nk.json_encode(state_payload(state, tick)), nil, nil)
end

local function send_event(dispatcher, event, targets)
    dispatcher.broadcast_message(OP_LOBBY_EVENT, nk.json_encode(event), targets, nil)
end

local function send_error(dispatcher, state, user_id, reason)
    local p = state.players[user_id]
    if p ~= nil and p.presence ~= nil then
        send_event(dispatcher, { kind = "error", error = reason }, { p.presence })
    end
end

local function notice(dispatcher, state, what, user_id)
    send_event(dispatcher, {
        kind = "notice",
        notice = what,
        user_id = user_id,
        username = username_of(state, user_id),
        name = name_of(state, user_id),
    }, nil)
end

local function build_label(state)
    local label = {
        kind = "ikemen-lobby",
        version = 2,
        game = state.game,
        build = state.build,
        visibility = state.settings.private and "private" or "public",
        players = player_count(state),
        size = state.settings.size,
        -- Kept for clients written against the first lobby protocol.
        max_players = MAX_PLAYERS,
        max_games = state.settings.max_games,
        format = state.settings.format,
    }
    if not state.settings.private then
        local names, tags = {}, {}
        for _, user_id in ipairs(ordered_players(state)) do
            names[#names + 1] = clean_text(name_of(state, user_id), 16)
            tags[#tags + 1] = tag_of(user_id)
        end
        label.name = state.settings.name
        label.comment = state.settings.comment
        label.code = state.code
        label.host = clean_text(name_of(state, state.host), 16)
        label.host_tag = state.host and tag_of(state.host) or ""
        label.tags = tags
        local host = state.host and state.players[state.host]
        label.region = host and host.region or ""
        label.phase = state.phase
        label.open = player_count(state) < state.settings.size and "yes" or "no"
        label.rounds = state.settings.rounds
        label.time = state.settings.time
        label.teams = state.settings.teams
        label.stage = state.settings.stage
        label.watch = state.settings.watch
        label.members = names
    end
    return nk.json_encode(label)
end

-- Sends state, pairing and label updates that the last changes require.
local function flush(dispatcher, state, tick)
    if state.dirty then
        state.dirty = false
        broadcast_state(dispatcher, state, tick)
    end
    if state.pairing_dirty then
        state.pairing_dirty = false
        broadcast_pairing(dispatcher, state)
    end
    if state.label_dirty then
        state.label_dirty = false
        local label = build_label(state)
        if label ~= state.label then
            state.label = label
            dispatcher.match_label_update(label)
        end
    end
end

-- ---------------------------------------------------------------------------
-- Room codes

local function new_code()
    local uuid = nk.uuid_v4():gsub("-", "")
    local out = {}
    for i = 1, CODE_LENGTH do
        local byte = tonumber(uuid:sub(i * 2 - 1, i * 2), 16) or 0
        local index = (byte % #CODE_ALPHABET) + 1
        out[#out + 1] = CODE_ALPHABET:sub(index, index)
    end
    return table.concat(out)
end

local function read_code(code)
    local ok, objects = pcall(nk.storage_read, {
        { collection = CODE_COLLECTION, key = code, user_id = nil },
    })
    if not ok or objects == nil or objects[1] == nil then
        return nil
    end
    local value = objects[1].value
    if type(value) ~= "table" then
        return nil
    end
    return value.match_id
end

local function delete_code(code)
    if code == nil or code == "" then
        return
    end
    pcall(nk.storage_delete, {
        { collection = CODE_COLLECTION, key = code, user_id = nil },
    })
end

-- ---------------------------------------------------------------------------
-- Commands from members

local function handle_command(dispatcher, state, tick, sender_id, command)
    local kind = command.kind
    local member = state.players[sender_id]
    if member == nil then
        return
    end
    local is_host = sender_id == state.host

    if kind == "ready" then
        local ready = command.ready == true
        if member.ready ~= ready then
            member.ready = ready
            if state.phase == "results" then
                -- Keep the announced pairing current; waiting recomputes it.
                state.next_match = compute_next(state)
            end
            state.dirty = true
        end
    elseif kind == "member" then
        local ping = tonumber(command.ping)
        if ping ~= nil then
            ping = int_in(ping, -1, -1, 9999)
            if ping ~= member.ping then
                member.ping = ping
                state.dirty = true
            end
        end
        local region = clean_text(command.region, 8)
        if region ~= nil and region ~= member.region then
            member.region = region
            state.dirty = true
            state.label_dirty = true
        end
        if command.connection ~= nil then
            local connection = one_of(command.connection, "", CONNECTIONS)
            if connection ~= member.connection then
                member.connection = connection
                state.dirty = true
            end
        end
    elseif kind == "link" then
        -- The round trip between the sender and another member, measured over
        -- their direct connection while they played each other.
        local peer = command.peer
        local rtt = tonumber(command.rtt)
        local current = state.current_match
        if type(peer) ~= "string" or rtt == nil or state.players[peer] == nil or current == nil or
            not ((current.player1 == sender_id and current.player2 == peer) or
                (current.player2 == sender_id and current.player1 == peer)) then
            return
        end
        rtt = int_in(rtt, 0, 0, LINK_LIMIT_MS)
        local key = pair_key(sender_id, peer)
        local link = state.links[key]
        if link == nil or link.rtt ~= rtt then
            state.links[key] = { a = sender_id, b = peer, rtt = rtt }
            state.dirty = true
        end
    elseif kind == "watching" then
        -- The member is (or stops) watching the current match.
        local watching = command.watching == true
        if (member.watching == true) ~= watching then
            member.watching = watching
            state.dirty = true
        end
    elseif kind == "watch" then
        -- The member wants the current match's stream from its beginning.
        local stream = state.stream
        if stream == nil or state.current_match == nil or stream.seq ~= state.match_seq or
            (command.seq ~= nil and tonumber(command.seq) ~= stream.seq) then
            send_error(dispatcher, state, sender_id, "There is no match to watch")
            return
        end
        if sender_id == stream.player1 or sender_id == stream.player2 then
            return
        end
        if stream.truncated then
            send_error(dispatcher, state, sender_id, "The match is too long to watch from its start")
            return
        end
        state.backfill[sender_id] = 1
    elseif kind == "chat" then
        local text = clean_text(command.text, CHAT_LIMIT)
        if text == nil or text == "" then
            return
        end
        if member.chat_tick ~= nil and tick - member.chat_tick < CHAT_INTERVAL_TICKS then
            send_error(dispatcher, state, sender_id, "Slow down")
            return
        end
        member.chat_tick = tick
        send_event(dispatcher, {
            kind = "chat",
            user_id = sender_id,
            username = member.username,
            name = member.name,
            text = text,
        }, nil)
    elseif kind == "settings" then
        if not is_host then
            send_error(dispatcher, state, sender_id, "Only the host can change settings")
            return
        end
        if state.phase == "playing" or state.phase == "results" or
            (not is_queue_format(state.settings.format) and state.tournament.started and not state.tournament.finished) then
            send_error(dispatcher, state, sender_id, "Settings can only change between rounds of play")
            return
        end
        local settings = normalize_settings(command.settings, state.settings)
        if settings.size < player_count(state) then
            send_error(dispatcher, state, sender_id, "The lobby has more members than that size")
            return
        end
        local format_changed = settings.format ~= state.settings.format
        state.settings = settings
        if format_changed then
            local t = state.tournament
            t.started = false
            t.finished = false
            t.winner = nil
            t.tied = nil
            t.streak_player = nil
            t.winner_streak = 0
            t.queue = ordered_players(state)
            state.started = false
            state.next_match = nil
            if state.phase == "finished" then
                set_phase(state, "waiting", tick)
            end
        end
        state.dirty = true
        state.label_dirty = true
        notice(dispatcher, state, "settings", sender_id)
    elseif kind == "start" then
        if not is_host then
            send_error(dispatcher, state, sender_id, "Only the host can start the lobby")
            return
        end
        if is_queue_format(state.settings.format) then
            state.started = true
        else
            local t = state.tournament
            if t.started and not t.finished then
                send_error(dispatcher, state, sender_id, "The tournament is already running")
                return
            end
            if not start_tournament(state) then
                send_error(dispatcher, state, sender_id, "At least two members must be ready")
                return
            end
            if state.phase == "finished" then
                set_phase(state, "waiting", tick)
            end
        end
        state.dirty = true
        notice(dispatcher, state, "start", sender_id)
    elseif kind == "kick" then
        local target = command.user_id
        if not is_host then
            send_error(dispatcher, state, sender_id, "Only the host can remove members")
            return
        end
        local p = target and state.players[target]
        if p == nil or target == sender_id then
            return
        end
        state.banned[target] = true
        state.kicked[target] = true
        send_event(dispatcher, { kind = "kicked" }, { p.presence })
        notice(dispatcher, state, "kicked", target)
        dispatcher.match_kick({ p.presence })
    elseif kind == "host" then
        local target = command.user_id
        if not is_host then
            send_error(dispatcher, state, sender_id, "Only the host can hand over the lobby")
            return
        end
        if target ~= nil and state.players[target] and target ~= sender_id then
            state.host = target
            state.dirty = true
            state.label_dirty = true
            notice(dispatcher, state, "host", target)
        end
    elseif kind == "pair_failed" or kind == "no_contest" then
        local current = state.current_match
        if current == nil or (sender_id ~= current.player1 and sender_id ~= current.player2) then
            return
        end
        if command.seq ~= nil and tonumber(command.seq) ~= state.match_seq then
            return
        end
        local reason = clean_text(command.reason, 24) or kind
        if reason == "cancel" then
            -- The player backed out of the connection: they sit out until they
            -- ready up again, and the pair stays eligible.
            member.ready = false
            no_contest(state, reason, false, tick)
        else
            no_contest(state, reason, kind == "pair_failed", tick)
        end
    end
end

-- ---------------------------------------------------------------------------
-- Match handlers

local function match_init(context, params)
    params = params or {}
    local settings = normalize_settings(params.settings or {
        name = params.name,
        format = params.format,
        max_games = (params.rules and params.rules.max_games) or params.max_games,
    }, DEFAULT_SETTINGS)
    local state = {
        creator = params.creator or context.user_id,
        host = params.creator or context.user_id,
        code = normalize_code(params.code),
        game = params.game or "",
        build = params.build or "",
        settings = settings,
        players = {},
        player_order = {},
        records = {},
        names = {},
        usernames = {},
        banned = {},
        kicked = {},
        failed_pairs = {},
        started = false,
        phase = "waiting",
        phase_tick = 0,
        results_until = nil,
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
            attempts = 0,
        },
        current_match = nil,
        next_match = nil,
        match_seq = 0,
        empty_ticks = 0,
        last_result = nil,
        version = 0,
        -- pair_key -> {a, b, rtt}: direct round trips members reported.
        links = {},
        -- The current match's replay stream: {seq, player1, player2, sender,
        -- messages = {{op, data}}}, and the members catching up on it
        -- (user_id -> index of the next stored message to send).
        stream = nil,
        backfill = {},
    }
    state.label = build_label(state)
    return state, TICK_RATE, state.label
end

local function match_join_attempt(context, dispatcher, tick, state, presence, metadata)
    metadata = metadata or {}
    local existing = state.players[presence.user_id]
    if state.banned[presence.user_id] then
        return state, false, "You were removed from this lobby"
    end
    if existing == nil and player_count(state) >= state.settings.size then
        return state, false, "Lobby is full"
    end
    if state.game ~= "" and tostring(metadata.game or "") ~= state.game then
        return state, false, "Lobby game identity does not match"
    end
    if state.build ~= "" and tostring(metadata.build or "") ~= state.build then
        return state, false, "Lobby build identity does not match"
    end
    if state.settings.private and existing == nil and normalize_code(metadata.code) ~= state.code then
        return state, false, "This lobby needs its room ID"
    end
    return state, true
end

local function match_join(context, dispatcher, tick, state, presences)
    for _, presence in ipairs(presences) do
        local user_id = presence.user_id
        local existing = state.players[user_id]
        -- The online name is the account's display name, read when the
        -- member joins; the username stands in until the player chooses one.
        -- Both are cut to 16 characters: a username can be up to 128 bytes.
        local display_name = clean_text(account_display_name(user_id), 16) or ""
        local name = display_name
        if name == "" then
            name = clean_text(presence.username, 16) or ""
        end
        state.usernames[user_id] = presence.username
        if existing ~= nil then
            -- The same account joined again (a new session replaces the old one).
            existing.session_id = presence.session_id
            existing.presence = presence
            existing.display_name = display_name
            existing.name = name
            state.names[user_id] = name
        else
            state.players[user_id] = {
                session_id = presence.session_id,
                username = presence.username,
                display_name = display_name,
                name = name,
                presence = presence,
                ready = false,
                ping = -1,
                region = "",
                connection = "",
                watching = false,
            }
            state.names[user_id] = name
            record_for(state, user_id)
            local known = false
            for _, id in ipairs(state.player_order) do
                if id == user_id then
                    known = true
                    break
                end
            end
            if not known then
                state.player_order[#state.player_order + 1] = user_id
            end
            if state.host == nil or not state.players[state.host] then
                state.host = user_id
            end
            queue_append(state, user_id)
            notice(dispatcher, state, "joined", user_id)
        end
    end
    state.dirty = true
    state.label_dirty = true
    schedule(state, tick)
    flush(dispatcher, state, tick)
    return state
end

local function match_leave(context, dispatcher, tick, state, presences)
    for _, presence in ipairs(presences) do
        local p = state.players[presence.user_id]
        -- An older session of an account that has joined again is ignored.
        if p ~= nil and p.session_id == presence.session_id then
            state.players[presence.user_id] = nil
            queue_remove(state, presence.user_id)
            forget_links(state, presence.user_id)
            state.backfill[presence.user_id] = nil
            if state.kicked[presence.user_id] then
                -- The "kicked" notice has already been sent.
                state.kicked[presence.user_id] = nil
            else
                notice(dispatcher, state, "left", presence.user_id)
            end
        end
    end
    if player_count(state) == 0 then
        delete_code(state.code)
        return nil
    end
    if state.host == nil or not state.players[state.host] then
        -- The member who joined earliest takes over.
        local ids = ordered_players(state)
        state.host = ids[1]
        notice(dispatcher, state, "host", state.host)
    end

    local t = state.tournament
    if t.streak_player ~= nil and not state.players[t.streak_player] then
        t.streak_player = nil
        t.winner_streak = 0
    end

    local current = state.current_match
    if current and (not state.players[current.player1] or not state.players[current.player2]) then
        local remaining = state.players[current.player1] and current.player1 or
            (state.players[current.player2] and current.player2 or nil)
        if is_queue_format(state.settings.format) then
            -- The remaining player keeps their turn at the front of the line.
            if remaining ~= nil then
                queue_prepend(state, remaining)
            end
            if t.streak_player == remaining then
                t.streak_player = nil
                t.winner_streak = 0
            end
            begin_results(state, tick, {
                seq = state.match_seq,
                player1 = current.player1,
                player2 = current.player2,
                no_contest = true,
                reason = "left",
            })
        elseif remaining ~= nil then
            -- Tournaments: the departed entrant forfeits the active pairing.
            local loser = remaining == current.player1 and current.player2 or current.player1
            advance_lobby_result(state, remaining, loser, tick)
            state.last_result.forfeit = true
        else
            state.current_match = nil
            set_phase(state, "waiting", tick)
        end
    end
    if state.next_match ~= nil and
        (not state.players[state.next_match.player1] or not state.players[state.next_match.player2]) then
        state.next_match = compute_next(state)
    end
    state.dirty = true
    state.label_dirty = true
    schedule(state, tick)
    flush(dispatcher, state, tick)
    return state
end

-- Delivers a P2P signal only to the member it names. Signals without a
-- receiver go to every member but the sender, as before.
local function relay_signal(dispatcher, state, message, sender_id)
    local ok, signal = pcall(nk.json_decode, message.data or "")
    if ok and type(signal) == "table" and type(signal.to) == "string" and signal.to ~= "" then
        local target = state.players[signal.to]
        if target ~= nil and target.presence ~= nil and signal.to ~= sender_id then
            dispatcher.broadcast_message(message.op_code, message.data, { target.presence }, message.sender)
        end
        return
    end
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

local function is_stream_op(op)
    return op == OP_REPLAY_HEADER or op == OP_REPLAY_CHUNK or op == OP_REPLAY_END or op == OP_REPLAY_RESET
end

-- A replay stream message (header, chunk, end or reset). The current
-- pairing's player 1, who hosts the netplay session, publishes: the first
-- header starts the match's stream, and the other messages belong to it. A
-- Turns match publishes one stream per character change: the later headers
-- continue the match's stored messages, so a late watcher gets the match
-- from its first stream on. The messages are kept for late watchers (within
-- a message and a byte limit) and relayed to every member but the two
-- players. Oversized messages are dropped. Nothing is relayed while the
-- lobby does not allow watching.
local function relay_stream(dispatcher, state, message, sender_id)
    if (state.settings.watch or 0) <= 0 then
        return
    end
    local op = message.op_code
    local size = #(message.data or "")
    if (op == OP_REPLAY_HEADER and size > STREAM_HEADER_MAX) or (op ~= OP_REPLAY_HEADER and size > STREAM_CHUNK_MAX) then
        return
    end
    local stream = state.stream
    local same_match = stream ~= nil and stream.seq == state.match_seq and stream.sender == sender_id
    if op == OP_REPLAY_HEADER and not same_match then
        -- The pairing's player 1 hosts the netplay session and publishes.
        local current = state.current_match
        if current == nil or sender_id ~= current.player1 then
            return
        end
        stream = {
            seq = state.match_seq,
            player1 = current.player1,
            player2 = current.player2,
            sender = sender_id,
            messages = {},
            bytes = 0,
        }
        state.stream = stream
        state.backfill = {}
        state.dirty = true
    elseif not same_match then
        return
    end
    if not stream.truncated and #stream.messages < STREAM_MESSAGE_LIMIT and stream.bytes + size <= STREAM_BYTES_LIMIT then
        stream.messages[#stream.messages + 1] = { op = op, data = message.data }
        stream.bytes = stream.bytes + size
    else
        -- A member who starts watching from here on could not get the whole
        -- match; the relay to the others goes on.
        stream.truncated = true
    end
    local targets = {}
    for user_id, player in pairs(state.players) do
        if user_id ~= stream.player1 and user_id ~= stream.player2 and player.presence ~= nil then
            targets[#targets + 1] = player.presence
        end
    end
    if #targets > 0 then
        dispatcher.broadcast_message(op, message.data, targets, message.sender)
    end
end

-- Sends members who asked to watch late the stored stream, a few messages per
-- tick, from its header on. Messages relayed meanwhile may reach them twice;
-- the client keeps one copy of each frame.
local function send_backfill(dispatcher, state)
    local stream = state.stream
    if stream == nil then
        state.backfill = {}
        return
    end
    for user_id, index in pairs(state.backfill) do
        local p = state.players[user_id]
        if p == nil or p.presence == nil then
            state.backfill[user_id] = nil
        else
            local sent = 0
            while index <= #stream.messages and sent < BACKFILL_PER_TICK do
                local m = stream.messages[index]
                dispatcher.broadcast_message(m.op, m.data, { p.presence }, nil)
                index = index + 1
                sent = sent + 1
            end
            if index > #stream.messages then
                state.backfill[user_id] = nil
            else
                state.backfill[user_id] = index
            end
        end
    end
end

local function match_loop(context, dispatcher, tick, state, messages)
    for _, message in ipairs(messages) do
        local sender_id = message.sender and message.sender.user_id or ""
        if message.op_code == OP_LOBBY_RESULT then
            local ok, command = pcall(nk.json_decode, message.data or "{}")
            local stale = ok and type(command) == "table" and command.seq ~= nil and
                tonumber(command.seq) ~= (state.match_seq or 0)
            if ok and type(command) == "table" and command.kind == "match_result" and not stale then
                if state.current_match ~= nil and
                    (sender_id == state.current_match.player1 or sender_id == state.current_match.player2) then
                    local result_ok, reason = advance_lobby_result(
                        state,
                        tostring(command.winner or ""),
                        tostring(command.loser or ""),
                        tick
                    )
                    if not result_ok then
                        send_error(dispatcher, state, sender_id, reason or "invalid result")
                    end
                end
            end
        elseif message.op_code == OP_LOBBY_COMMAND then
            local ok, command = pcall(nk.json_decode, message.data or "{}")
            if ok and type(command) == "table" then
                handle_command(dispatcher, state, tick, sender_id, command)
            end
        elseif message.op_code == OP_P2P_SIGNAL then
            relay_signal(dispatcher, state, message, sender_id)
        elseif is_stream_op(message.op_code) then
            relay_stream(dispatcher, state, message, sender_id)
        else
            -- Other data goes to everyone but the sender.
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
    end

    if state.phase == "playing" and tick - (state.phase_tick or tick) > MATCH_TIMEOUT_TICKS then
        no_contest(state, "timeout", false, tick)
    end
    send_backfill(dispatcher, state)
    schedule(state, tick)
    flush(dispatcher, state, tick)

    if player_count(state) == 0 then
        state.empty_ticks = state.empty_ticks + 1
        if state.empty_ticks > EMPTY_TIMEOUT_TICKS then
            delete_code(state.code)
            return nil
        end
    else
        state.empty_ticks = 0
    end
    return state
end

local function match_terminate(context, dispatcher, tick, state, grace_seconds)
    delete_code(state.code)
    return nil
end

local function match_signal(context, dispatcher, tick, state, data)
    return state, data
end

-- ---------------------------------------------------------------------------
-- RPCs

-- ikemen_lobby_create: {settings, game, build} -> {match_id, code}. The
-- creator joins the returned match afterwards. The first lobby protocol's
-- top-level name, format, max_games, max_players and rules.{max_games,
-- rounds} fill in settings that are not given.
local function create_lobby(context, payload)
    local params = nk.json_decode(payload or "{}") or {}
    local input = type(params.settings) == "table" and params.settings or {}
    local rules = type(params.rules) == "table" and params.rules or {}
    if input.name == nil then
        input.name = params.name
    end
    if input.format == nil then
        input.format = params.format
    end
    if input.max_games == nil then
        input.max_games = rules.max_games or params.max_games
    end
    if input.size == nil then
        input.size = params.max_players
    end
    if input.rounds == nil then
        input.rounds = rules.rounds
    end
    local settings = normalize_settings(input, DEFAULT_SETTINGS)

    local code = nil
    for _ = 1, 10 do
        local candidate = new_code()
        if read_code(candidate) == nil then
            code = candidate
            break
        end
    end
    if code == nil then
        error({ "Could not allocate a room ID", 13 })
    end

    local match_id = nk.match_create("ikemen_lobby", {
        creator = context.user_id,
        game = params.game or "",
        build = params.build or "",
        settings = settings,
        code = code,
    })
    nk.storage_write({
        {
            collection = CODE_COLLECTION,
            key = code,
            user_id = nil,
            value = { match_id = match_id },
            permission_read = 0,
            permission_write = 0,
        },
    })
    return nk.json_encode({ match_id = match_id, code = code })
end

-- ikemen_lobby_find: {code} -> {match_id}. Private lobbies are only
-- reachable this way, and joining one still needs the code.
local function find_lobby(context, payload)
    local params = nk.json_decode(payload or "{}") or {}
    local code = normalize_code(params.code)
    if code == "" then
        error({ "Enter a room ID", 3 })
    end
    local match_id = read_code(code)
    if match_id == nil then
        error({ "No lobby has that room ID", 5 })
    end
    local ok, match = pcall(nk.match_get, match_id)
    if not ok or match == nil then
        delete_code(code)
        error({ "No lobby has that room ID", 5 })
    end
    return nk.json_encode({ match_id = match_id, code = code })
end

nk.register_rpc(create_lobby, "ikemen_lobby_create")
nk.register_rpc(find_lobby, "ikemen_lobby_find")

return {
    match_init = match_init,
    match_join_attempt = match_join_attempt,
    match_join = match_join,
    match_leave = match_leave,
    match_loop = match_loop,
    match_terminate = match_terminate,
    match_signal = match_signal,
}
