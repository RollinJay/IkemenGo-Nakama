local nk = require("nakama")
local ranked = require("ikemen_ranked")

-- IKEMEN GO Elo storage and ranked set results.
--
-- Ratings change once per ranked set (a best-of-N series in a ranked
-- session, ikemen_ranked.lua), never per match. When a set is decided, each
-- player's game claims its winner (ikemen_submit_result). The server accepts
-- a claim only for a set it recorded, from one of the set's two players,
-- while the set is open. Two claims that name the same winner settle the
-- set: one storage transaction writes a settlement marker, both new ratings
-- and the set's record, so retries and simultaneous claims settle a set
-- once. Claims that disagree mark the set disputed, for a moderator
-- (ikemen_moderation.lua), who can also settle it.
--
-- This does not prove who won: two players who agree on a false result are
-- believed. Moderators can watch the set's replays (ikemen_ranked.lua).
--
-- ikemen_submit_result: {match_id, set, winner} -> {accepted, settled, status,
-- rating, opponent_rating}. match_id is the ranked session's match id, set
-- the set's number in the session (1 by default), and winner the user id of
-- the set's winner. The first version's fields still work: result (1: the
-- caller won, 0: the caller lost) in place of winner, opponent_id (checked
-- when given), game (checked when given) and replay_hash (claims whose
-- hashes differ disagree).
--
-- Errors (Nakama RPC error codes):
--   3 INVALID_ARGUMENT     a field is missing or does not fit the set
--   5 NOT_FOUND            the server made no ranked session (or set) with this id
--   7 PERMISSION_DENIED    the caller was not one of the set's players
--   9 FAILED_PRECONDITION  the match is not ranked (an unranked session, a
--                          lobby), the set takes no more claims (settled,
--                          disputed or void), or the caller already claimed
--                          the other winner
--  10 ABORTED              the set kept changing meanwhile; retry
--  16 UNAUTHENTICATED      no player's session (claims come from players)

local DEFAULTS = {
    initial_rating = 1000,
    k_factor = 32,
    rating_scale = 400,
}

local GAME_CONFIG = {
    -- ["my_game"] = { initial_rating = 1000, k_factor = 32, rating_scale = 400 },
}

local function config_for(game)
    return GAME_CONFIG[game] or DEFAULTS
end

local function rating_key(game)
    return game
end

-- Returns the rating record plus the storage version ("*" when absent) so the
-- settlement write can use optimistic concurrency. games, wins and losses
-- count ranked sets.
local function read_rating(user_id, game)
    local cfg = config_for(game)
    local records = nk.storage_read({{
        collection = "ikemen_rating",
        key = rating_key(game),
        user_id = user_id,
    }})
    local record = { rating = cfg.initial_rating, wins = 0, losses = 0, games = 0 }
    if #records == 0 then
        return record, "*"
    end
    local value = records[1].value
    if type(value) == "table" then
        record.rating = tonumber(value.rating) or cfg.initial_rating
        record.wins = tonumber(value.wins) or 0
        record.losses = tonumber(value.losses) or 0
        record.games = tonumber(value.games) or 0
    end
    return record, records[1].version
end

local function rating_object(user_id, game, record, version)
    return {
        collection = "ikemen_rating",
        key = rating_key(game),
        user_id = user_id,
        value = record,
        version = version,
        permission_read = 1,
        permission_write = 0,
    }
end

local function is_settled(id)
    local marker = nk.storage_read({{ collection = "ikemen_settlement", key = id }})
    return #marker > 0
end

-- Settles a set read at version: winner_id wins it. One storage transaction
-- inserts the system-owned ikemen_settlement/<set id> marker (version "*":
-- only if absent), writes both players' new ratings over the versions read,
-- and writes the set's record (status settled) over version. Returns true and
-- the winner's and the loser's new ratings; "settled" when the set was
-- settled meanwhile; "changed" when its record changed meanwhile (the caller
-- reads it again and decides again).
local function settle_set(value, version, winner_id, by)
    local game = value.game or ""
    local loser_id = nil
    for _, p in ipairs(value.players) do
        if p.user_id ~= winner_id then
            loser_id = p.user_id
        end
    end
    if loser_id == nil then
        error({ "the set has no second player", 3 })
    end
    local cfg = config_for(game)
    for _ = 1, 3 do
        local w, w_version = read_rating(winner_id, game)
        local l, l_version = read_rating(loser_id, game)
        local expected_w = 1 / (1 + math.pow(10, (l.rating - w.rating) / cfg.rating_scale))
        local expected_l = 1 - expected_w
        w.rating = w.rating + cfg.k_factor * (1 - expected_w)
        l.rating = l.rating + cfg.k_factor * (0 - expected_l)
        w.games, l.games = w.games + 1, l.games + 1
        w.wins, l.losses = w.wins + 1, l.losses + 1

        value.status = "settled"
        value.reason = ""
        value.winner = winner_id
        value.settled_by = by
        value.decided = ranked.now()
        local ok = pcall(nk.storage_write, {
            {
                collection = "ikemen_settlement",
                key = value.match_id,
                value = { players = { value.players[1].user_id, value.players[2].user_id }, game = game, winner = winner_id },
                version = "*",
                permission_read = 0,
                permission_write = 0,
            },
            rating_object(winner_id, game, w, w_version),
            rating_object(loser_id, game, l, l_version),
            ranked.set_object(value, version),
        })
        if ok then
            nk.logger_info(string.format("ikemen ranked: set %s settled by %s: %s won (%.1f), %s lost (%.1f)",
                value.match_id, by, winner_id, w.rating, loser_id, l.rating))
            return true, w, l
        end
        if is_settled(value.match_id) then
            return "settled"
        end
        local _, now_version = ranked.read_set(value.match_id)
        if now_version ~= version then
            return "changed"
        end
        -- Otherwise a rating changed underneath us (another set); retry.
    end
    error({ "IKEMEN result settlement conflicted; retry", 10 })
end

-- Tells the session that both players claimed its set, which is decided (it
-- then stores the players' replays of the set as soon as their streams end).
-- A first claim does not: a player can claim during the set, and then leave
-- it early.
local function signal_session(value)
    if type(value.session) == "string" and value.session ~= "" then
        pcall(nk.match_signal, value.session, nk.json_encode({ kind = "ranked_claim", set = tonumber(value.set) or 1 }))
    end
end

local function submit_result(context, payload)
    local user_id = context.user_id
    if user_id == nil or user_id == "" then
        error({ "a result is claimed by a player: call this RPC with the player's session", 16 })
    end
    local ok, data = pcall(nk.json_decode, payload or "{}")
    if not ok or type(data) ~= "table" then
        error({ "invalid IKEMEN result submission", 3 })
    end
    local match_id = tostring(data.match_id or "")
    if match_id == "" then
        error({ "match_id is required", 3 })
    end
    local set = tonumber(data.set) or 1
    local id = ranked.set_id(match_id, set)
    local hash = tostring(data.replay_hash or "")

    local value = ranked.read_set(id)
    if value == nil then
        error(ranked.no_set_error(match_id, set))
    end
    if ranked.player_index(value, user_id) == nil then
        error({ string.format("you were not a player of ranked set %s", id), 7 })
    end
    local other_id = nil
    for _, p in ipairs(value.players) do
        if p.user_id ~= user_id then
            other_id = p.user_id
        end
    end
    if data.opponent_id ~= nil and tostring(data.opponent_id) ~= other_id then
        error({ "opponent_id is not your opponent in this set", 3 })
    end
    if data.game ~= nil and tostring(data.game) ~= tostring(value.game) then
        error({ string.format("the set is of game %q, not %q", tostring(value.game), tostring(data.game)), 3 })
    end
    local winner = data.winner
    if winner == nil then
        local result = tonumber(data.result)
        if result == 1 then
            winner = user_id
        elseif result == 0 then
            winner = other_id
        end
    end
    winner = tostring(winner or "")
    if winner == "" or ranked.player_index(value, winner) == nil then
        error({ "winner must be the user id of one of the set's two players", 3 })
    end

    for _ = 1, 8 do
        local version
        value, version = ranked.read_set(id)
        if value == nil then
            error(ranked.no_set_error(match_id, set))
        end
        local now = ranked.now()
        local changed = ranked.timed_out(context, value, now)
        local mine = value.claims[user_id]
        if value.status ~= "open" then
            if changed then
                pcall(nk.storage_write, { ranked.set_object(value, version) })
            end
            if type(mine) == "table" and mine.winner == winner then
                -- A retry of a claim that counted.
                return nk.json_encode({ accepted = true, settled = value.status == "settled", status = value.status,
                    match_id = id, rating = (read_rating(user_id, value.game)).rating })
            end
            error({ string.format("ranked set %s is %s: it takes no more claims", id, value.status), 9 })
        end
        if type(mine) == "table" and mine.winner ~= winner then
            error({ "your claim for this set is already recorded, with the other winner", 9 })
        end
        local repeated = type(mine) == "table"
        if not repeated then
            value.claims[user_id] = { user_id = user_id, winner = winner, at = now, replay_hash = hash }
        end
        local theirs = value.claims[other_id]
        if type(theirs) ~= "table" then
            if repeated and not changed then
                -- A retry while the other claim has not come: nothing to write.
                return nk.json_encode({ accepted = true, settled = false, status = "open", match_id = id })
            end
            if pcall(nk.storage_write, { ranked.set_object(value, version) }) then
                return nk.json_encode({ accepted = true, settled = false, status = "open", match_id = id })
            end
        elseif theirs.winner ~= winner or (hash ~= "" and tostring(theirs.replay_hash or "") ~= "" and theirs.replay_hash ~= hash) then
            value.status = "disputed"
            value.reason = theirs.winner ~= winner and "claims_disagree" or "replay_hash_disagree"
            value.decided = now
            if pcall(nk.storage_write, { ranked.set_object(value, version) }) then
                signal_session(value)
                nk.logger_info(string.format("ikemen ranked: set %s disputed (%s)", id, value.reason))
                return nk.json_encode({ accepted = true, settled = false, status = "disputed", reason = value.reason, match_id = id })
            end
        else
            local result, w, l = settle_set(value, version, winner, "players")
            if result == true then
                signal_session(value)
                local mine_rating, their_rating = w, l
                if winner ~= user_id then
                    mine_rating, their_rating = l, w
                end
                return nk.json_encode({ accepted = true, settled = true, status = "settled", match_id = id,
                    rating = mine_rating.rating, opponent_rating = their_rating.rating })
            end
            -- "settled" or "changed": read the set again.
        end
    end
    error({ "the ranked set kept changing while the claim was recorded; retry", 10 })
end

local function get_rating(context, payload)
    local data = nk.json_decode(payload or "{}") or {}
    local game = tostring(data.game or "")
    if game == "" then
        error("game is required")
    end
    local record = read_rating(context.user_id, game)
    return nk.json_encode(record)
end

nk.register_rpc(submit_result, "ikemen_submit_result")
nk.register_rpc(get_rating, "ikemen_get_rating")

return {
    read_rating = read_rating,
    settle_set = settle_set,
}
