local nk = require("nakama")
local ranked = require("ikemen_ranked")
local rating = require("ikemen_rating")

-- IKEMEN GO ranked sets for moderators (ikemen_ranked.lua describes the
-- sets and their replays). These RPCs are for the server's operators, not
-- for players: they are called server to server, with Nakama's runtime
-- http_key (POST /v2/rpc/<id>?http_key=<key>&unwrap), and refuse any call
-- that carries a player's session.
--
-- ikemen_ranked_list {status, limit, cursor, match_id} -> {sets, cursor}
--   status: disputed (default), open, settled, void or all; limit: 1 to 100
--   (default 20); cursor: the cursor of the previous page. Each set is the
--   record as ikemen_ranked.lua describes it, with claims ({user_id,
--   winner, at}), left_early (user ids) and replays ({user_id, bytes,
--   complete}) as lists in the players' order, each player with their
--   current rating, level and level_text and, once the set has settled,
--   their result in it (result: won, rating_before, rating, level_before,
--   level_before_text, level, level_text, change, change_text and label,
--   as the record kept them; ikemen_rating.lua), and the set with its
--   game's label (ikemen_levels.lua). Newest first; cursor is ""
--   after the last page (a page can hold fewer than limit sets: one call
--   reads at most 50 pages of the index). With match_id (a set id), the list
--   holds that set alone, whatever its status unless status is given. A set
--   the list finds overdue (ikemen_ranked.timed_out) is written so, and when
--   it becomes disputed, its players are told (ikemen_rating.lua).
--
-- ikemen_ranked_replay {match_id, user_id} -> {file_name, replay, user_id,
--   complete, matches, bytes}
--   A player's kept replay of the set (match_id: the set id the list gives)
--   as a match replay file that IKEMEN GO plays (WATCH MODE > REPLAY, or
--   Ikemen_GO -replay <file>), base64-encoded. Without user_id, the first
--   complete replay in the players' order, or else the longest. matches is
--   the number of matches the file holds.
--
-- ikemen_ranked_settle {match_id, winner | void = true, note} -> {status}
--   Settles the set for winner (a player's user id), with the one rating
--   change, the settlement marker, the leaderboard entries and the
--   notifications of a settlement by its players, or voids it (no rating
--   change, no notification). note is kept on the record (at most 500
--   characters). A set that is settled or void already is refused.
--
-- Errors: 3 INVALID_ARGUMENT, 5 NOT_FOUND (no such set, or no replay kept),
-- 7 PERMISSION_DENIED (a player's session), 9 FAILED_PRECONDITION (settled
-- or void already), 10 ABORTED (the set kept changing; retry).

local STATUSES = { disputed = true, open = true, settled = true, void = true, all = true }
local NOTE_MAX = 500
-- Storage pages read by one list call at most.
local LIST_PAGES = 50

local function server_only(context)
    if context.user_id ~= nil and context.user_id ~= "" then
        error({ "this RPC is for the server's moderators: call it with the server's http_key, not a player's session", 7 })
    end
end

-- The characters (Unicode code points) of UTF-8 text: its bytes other than
-- continuation bytes.
local function characters(text)
    local n = 0
    for i = 1, #text do
        local b = string.byte(text, i)
        if b < 0x80 or b >= 0xC0 then
            n = n + 1
        end
    end
    return n
end

local function decode(payload)
    if payload == nil or payload == "" then
        return {}
    end
    local ok, data = pcall(nk.json_decode, payload)
    if not ok or type(data) ~= "table" then
        error({ "the payload must be a JSON object", 3 })
    end
    return data
end

-- Adds each player's current rating, level and level_text, the texts of
-- their result in the set, and the game's label, to the sets of a list
-- (public_set records). One storage read.
local function add_levels(context, sets)
    local reads = {}
    for _, set in ipairs(sets) do
        for _, p in ipairs(set.players) do
            if p.user_id ~= nil and p.user_id ~= "" and set.game ~= "" then
                reads[#reads + 1] = { collection = "ikemen_rating", key = set.game, user_id = p.user_id }
            end
        end
    end
    local found = {}
    if #reads > 0 then
        for _, o in ipairs(nk.storage_read(reads)) do
            if type(o.value) == "table" and tonumber(o.value.rating) ~= nil then
                found[o.key .. "\n" .. o.user_id] = tonumber(o.value.rating)
            end
        end
    end
    for _, set in ipairs(sets) do
        local initial = rating.config_for(set.game).initial_rating
        for _, p in ipairs(set.players) do
            p.rating = found[tostring(set.game) .. "\n" .. tostring(p.user_id)] or initial
            local level = rating.level_fields(context, set.game, p.rating, {})
            p.level, p.level_text = level.level, level.level_text
            set.label = level.label
            if p.result ~= nil then
                p.result = rating.result_fields(context, set.game, p.result)
            end
        end
        if set.label == nil then
            set.label = rating.level_fields(context, set.game, initial, {}).label
        end
    end
    return sets
end

local function ranked_list(context, payload)
    server_only(context)
    local data = decode(payload)
    local status = tostring(data.status or "disputed")
    if not STATUSES[status] then
        error({ "status must be disputed, open, settled, void or all", 3 })
    end
    local limit = math.floor(tonumber(data.limit) or 20)
    if limit < 1 or limit > 100 then
        error({ "limit must be 1 to 100", 3 })
    end
    local cursor = tostring(data.cursor or "")
    ranked.sweep(context, 20)
    local sets = {}
    local now = ranked.now()
    if data.match_id ~= nil then
        local id = tostring(data.match_id)
        local wrote = false
        local value = ranked.update_set(id, function(v)
            wrote = ranked.timed_out(context, v, now)
            return wrote
        end)
        if value ~= nil and wrote and value.status == "disputed" then
            rating.notify_disputed(context, value)
        end
        if value ~= nil and (data.status == nil or status == "all" or value.status == status) then
            sets[1] = ranked.public_set(value)
        end
        return ranked.encode({ sets = add_levels(context, sets), cursor = "" })
    end
    -- The index lists the sets newest first; a page of it names the sets
    -- to read.
    local pages = 0
    repeat
        local ok, entries, next_cursor = pcall(ranked.list_system, ranked.INDEX, limit - #sets, cursor)
        if not ok then
            if cursor ~= "" then
                error({ "cursor is not a cursor this list gave", 3 })
            end
            error(entries)
        end
        local reads = {}
        for _, e in ipairs(entries or {}) do
            local id = type(e.value) == "table" and e.value.match_id
            if type(id) == "string" and id ~= "" then
                reads[#reads + 1] = { collection = ranked.SETS, key = id }
            end
        end
        local by_key = {}
        if #reads > 0 then
            for _, o in ipairs(nk.storage_read(reads)) do
                by_key[o.key] = o
            end
        end
        for _, r in ipairs(reads) do
            local o = by_key[r.key]
            if o ~= nil and type(o.value) == "table" then
                local value = ranked.normalize(o.value)
                if ranked.timed_out(context, value, now) and pcall(nk.storage_write, { ranked.set_object(value, o.version) })
                    and value.status == "disputed" then
                    rating.notify_disputed(context, value)
                end
                if status == "all" or value.status == status then
                    sets[#sets + 1] = ranked.public_set(value)
                end
            end
        end
        cursor = next_cursor or ""
        pages = pages + 1
    until #sets >= limit or cursor == "" or pages >= LIST_PAGES
    return ranked.encode({ sets = add_levels(context, sets), cursor = cursor })
end

local function ranked_replay(context, payload)
    server_only(context)
    local data = decode(payload)
    local id = tostring(data.match_id or "")
    if id == "" then
        error({ "match_id is required", 3 })
    end
    local value = ranked.read_set(id)
    if value == nil then
        error({ "no ranked set " .. id, 5 })
    end
    local candidates = {}
    if data.user_id ~= nil and data.user_id ~= "" then
        local uid = tostring(data.user_id)
        if ranked.player_index(value, uid) == nil then
            error({ "user " .. uid .. " was not a player of set " .. id, 3 })
        end
        candidates[1] = uid
    else
        for _, p in ipairs(value.players) do
            candidates[#candidates + 1] = p.user_id
        end
    end
    local best, why = nil, "none"
    for _, uid in ipairs(candidates) do
        local replay, reason = ranked.read_replay(context, id, uid)
        if replay ~= nil then
            if best == nil or (replay.complete and not best.complete)
                or (replay.complete == best.complete and not best.complete and (tonumber(replay.bytes) or 0) > (tonumber(best.bytes) or 0)) then
                best = replay
            end
        elseif reason == "expired" then
            why = "expired"
        end
    end
    if best == nil then
        if why == "expired" then
            error({ "the replay of set " .. id .. " has expired and was removed", 5 })
        end
        error({ "no replay is kept for set " .. id .. (data.user_id and (" and user " .. tostring(data.user_id)) or ""), 5 })
    end
    local text, matches = ranked.replay_file(best)
    return nk.json_encode({
        file_name = ranked.replay_file_name(value, best),
        replay = nk.base64_encode(text),
        user_id = best.user_id,
        complete = best.complete == true,
        matches = matches,
        bytes = #text,
    })
end

local function ranked_settle(context, payload)
    server_only(context)
    local data = decode(payload)
    local id = tostring(data.match_id or "")
    if id == "" then
        error({ "match_id is required", 3 })
    end
    local void = data.void == true
    local winner = data.winner ~= nil and tostring(data.winner) or ""
    if void == (winner ~= "") then
        error({ "give either winner (a player's user id) or void = true", 3 })
    end
    local note = data.note ~= nil and tostring(data.note) or nil
    -- A character has at most 4 bytes, so a longer note is not counted.
    if note ~= nil and (#note > 4 * NOTE_MAX or characters(note) > NOTE_MAX) then
        error({ string.format("note has more than %d characters", NOTE_MAX), 3 })
    end
    for _ = 1, 8 do
        local value, version = ranked.read_set(id)
        if value == nil then
            error({ "no ranked set " .. id, 5 })
        end
        ranked.timed_out(context, value)
        if value.status == "settled" or value.status == "void" then
            error({ string.format("ranked set %s is %s already", id, value.status), 9 })
        end
        if note ~= nil then
            value.note = note
        end
        if void then
            value.status = "void"
            value.reason = "moderator"
            value.settled_by = "moderator"
            value.decided = ranked.now()
            if pcall(nk.storage_write, { ranked.set_object(value, version) }) then
                nk.logger_info("ikemen ranked: set " .. id .. " voided by a moderator")
                return nk.json_encode({ status = "void", match_id = id })
            end
        else
            if ranked.player_index(value, winner) == nil then
                error({ "winner must be the user id of one of the set's players", 3 })
            end
            local result = rating.settle_set(context, value, version, winner, "moderator")
            if result == true then
                return nk.json_encode({ status = "settled", match_id = id, winner = winner })
            end
            -- "settled" or "changed": read the set again.
        end
    end
    error({ "the ranked set kept changing; retry", 10 })
end

nk.register_rpc(ranked_list, "ikemen_ranked_list")
nk.register_rpc(ranked_replay, "ikemen_ranked_replay")
nk.register_rpc(ranked_settle, "ikemen_ranked_settle")
