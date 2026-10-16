local nk = require("nakama")
local ranked = require("ikemen_ranked")
local levels = require("ikemen_levels")

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
-- Players see a level computed from their rating (ikemen_levels.lua), which
-- the answers below carry next to it: level, level_text and label.
--
-- Once a set has settled, both players' entries on the game's leaderboard
-- are written, and each player is sent a notification (subject
-- ikemen_ranked_settled) with their level before and after. A disputed set
-- sends both players ikemen_ranked_disputed instead; a void set sends
-- nothing. Neither the leaderboard nor the notifications can undo a
-- settlement: their failures are logged.
--
-- ikemen_submit_result: {match_id, set, winner} -> {accepted, settled, status,
-- match_id, rating, level, level_text, label, opponent_rating,
-- opponent_level, opponent_level_text}. match_id is the ranked session's
-- match id, set the set's number in the session (1 by default), and winner
-- the user id of the set's winner. The ratings and levels come once the set
-- is settled (the caller's alone in the answer to a claim sent again). The
-- first version's fields still work: result (1: the caller won, 0: the
-- caller lost) in place of winner, opponent_id (checked when given), game
-- (checked when given) and replay_hash (claims whose hashes differ
-- disagree).
--
-- ikemen_get_rating: {game} -> {rating, games, wins, losses, level,
-- level_text, label}: the caller's rating. It also writes the caller's
-- leaderboard entry when the caller has played a ranked set and the entry
-- is missing or older (a rating from before the leaderboards, or a write
-- that failed).
--
-- ikemen_leaderboard: {game, limit, cursor} -> {label, records, cursor, mine}
-- The game's leaderboard, highest rating first: limit 1 to 100 records (20
-- by default), cursor the cursor of the previous page ("" after the last
-- page). Each record is {rank, user_id, name, level, level_text, wins,
-- losses}, name the player's online name (the account's display name, or
-- its username until the player chooses one; at most 16 characters). mine
-- is the caller's own record, left out when the caller has none or the
-- call has no player's session (a call with the server's http_key). A game
-- whose leaderboard does not exist yet (no set has settled) has no
-- records.
--
-- The leaderboards are Nakama leaderboards, ikemen_rating.<game>, created
-- when a game's first set settles: authoritative (players cannot write to
-- them), highest score first, with each player's rating times 1000, rounded,
-- as the score (at least 0; finer than the rating rounded, so that the order
-- follows the levels, which come from the exact rating) and the sets won as
-- the subscore; the record's metadata has the rating itself, wins, losses
-- and games. Levels are computed when the leaderboard is read.
--
-- Notifications (Nakama notifications, persistent: a player who is not
-- connected finds them with Nakama's notification list):
--   ikemen_ranked_settled (code 4101): {match_id (the set id), session, set,
--     game, won, settled_by, label, rating_before, rating, level_before,
--     level_before_text, level, level_text, change, change_text, wins,
--     losses}. change is level - level_before; change_text has its sign
--     (+1,239, -1,239, +0).
--   ikemen_ranked_disputed (code 4102): {match_id, session, set, game,
--     reason}: the claims disagree (claims_disagree, replay_hash_disagree),
--     or the second claim did not come in time (claim_timeout); a moderator
--     decides.
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
-- ikemen_get_rating and ikemen_leaderboard refuse a payload without game,
-- and ikemen_leaderboard a limit out of range or a game id over 114 bytes,
-- with 3 INVALID_ARGUMENT.

-- The Elo settings of each game: the rating a new player starts with, the
-- K-factor and the scale. The matchmaker reads the ratings with these too.
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

-- Notification subjects and codes.
local SETTLED = "ikemen_ranked_settled"
local DISPUTED = "ikemen_ranked_disputed"
local SETTLED_CODE = 4101
local DISPUTED_CODE = 4102

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

-- A game's level of an Elo rating (ikemen_levels.lua): its start is the
-- level of the rating new players start with.
local function level_of(context, game, elo)
    return levels.level_of(levels.entry(context, game), elo - config_for(game).initial_rating + 1000)
end

-- Adds level, level_text and label of the Elo rating elo to out.
local function level_fields(context, game, elo, out)
    return levels.fields(context, game, elo - config_for(game).initial_rating + 1000, out)
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

-- ---------------------------------------------------------------------------
-- Leaderboards

local function leaderboard_id(game)
    return "ikemen_rating." .. tostring(game or "")
end

-- The leaderboards this Lua runtime knows to exist (creating one that exists
-- changes nothing).
local created = {}

local function ensure_leaderboard(id)
    if created[id] then
        return
    end
    nk.leaderboard_create(id, true, "desc", "set", nil, { kind = "ikemen_rating" }, true)
    created[id] = true
end

-- Writes a player's entry on the game's leaderboard from their rating
-- record. Raises an error when it cannot.
local function write_entry(user_id, username, game, record)
    local id = leaderboard_id(game)
    local function write()
        nk.leaderboard_record_write(id, user_id, username or "", math.max(0, math.floor(record.rating * 1000 + 0.5)),
            math.max(0, math.floor(record.wins)), {
                rating = record.rating,
                wins = record.wins,
                losses = record.losses,
                games = record.games,
            })
    end
    ensure_leaderboard(id)
    if not pcall(write) then
        -- The leaderboard may have been deleted since this runtime made it.
        created[id] = nil
        ensure_leaderboard(id)
        write()
    end
end

-- Writes the entry of a player who has played a ranked set from their
-- rating record when the entry is missing, or older than the record (it
-- counts fewer sets): a rating from before the leaderboards, a write that
-- failed, or a settlement that committed before another one of the
-- player's. Returns true when it wrote the entry.
local function sync_entry(user_id, username, game, record)
    if record.games <= 0 then
        return false
    end
    local id = leaderboard_id(game)
    local existing = nil
    if #nk.leaderboards_get_id({ id }) > 0 then
        local _, owners = nk.leaderboard_records_list(id, { user_id }, 0, nil, 0)
        existing = owners and owners[1]
    end
    local metadata = existing and type(existing.metadata) == "table" and existing.metadata or {}
    if existing == nil or (tonumber(metadata.games) or 0) < record.games then
        write_entry(user_id, username, game, record)
        return true
    end
    return false
end

-- ---------------------------------------------------------------------------
-- Notifications

local function notify(user_id, subject, content, code)
    local ok, err = pcall(nk.notification_send, user_id, subject, content, code, nil, true)
    if not ok then
        nk.logger_error(string.format("ikemen ranked: cannot send %s to %s: %s", subject, tostring(user_id), tostring(err)))
    end
end

-- Tells both players of a set that it is disputed (once: the caller has just
-- written the set's change to disputed).
local function notify_disputed(context, value)
    for _, p in ipairs(value.players or {}) do
        if type(p) == "table" and type(p.user_id) == "string" and p.user_id ~= "" then
            notify(p.user_id, DISPUTED, {
                match_id = value.match_id,
                session = value.session or value.match_id,
                set = tonumber(value.set) or 1,
                game = value.game or "",
                reason = value.reason or "",
            }, DISPUTED_CODE)
        end
    end
end

-- After a set has settled: both players' leaderboard entries, then their
-- notifications. results: {user_id, username, before, after, won} of each
-- player. Each entry is written from the player's rating as stored now, and
-- not when it already counts as many sets: another settlement of the
-- player's that committed meanwhile may have written a newer one.
local function after_settlement(context, value, results)
    local game = value.game or ""
    for _, r in ipairs(results) do
        local ok, err = pcall(function()
            sync_entry(r.user_id, r.username, game, (read_rating(r.user_id, game)))
        end)
        if not ok then
            nk.logger_error(string.format("ikemen ranked: set %s settled, but the leaderboard entry of %s was not written: %s",
                value.match_id, r.user_id, tostring(err)))
        end
    end
    local label = levels.label(context, game)
    for _, r in ipairs(results) do
        local before = level_of(context, game, r.before.rating)
        local after = level_of(context, game, r.after.rating)
        notify(r.user_id, SETTLED, {
            match_id = value.match_id,
            session = value.session or value.match_id,
            set = tonumber(value.set) or 1,
            game = game,
            won = r.won,
            settled_by = value.settled_by,
            label = label,
            rating_before = r.before.rating,
            rating = r.after.rating,
            level_before = before,
            level_before_text = levels.format(before),
            level = after,
            level_text = levels.format(after),
            change = after - before,
            change_text = levels.change_text(after - before),
            wins = r.after.wins,
            losses = r.after.losses,
        }, SETTLED_CODE)
    end
end

local function username_in(value, user_id)
    for _, p in ipairs(value.players or {}) do
        if type(p) == "table" and p.user_id == user_id then
            return p.username or ""
        end
    end
    return ""
end

-- Settles a set read at version: winner_id wins it. One storage transaction
-- inserts the system-owned ikemen_settlement/<set id> marker (version "*":
-- only if absent), writes both players' new ratings over the versions read,
-- and writes the set's record (status settled) over version. Then both
-- players' leaderboard entries are written and their notifications sent.
-- Returns true and the winner's and the loser's new ratings; "settled" when
-- the set was settled meanwhile; "changed" when its record changed meanwhile
-- (the caller reads it again and decides again).
local function settle_set(context, value, version, winner_id, by)
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
        local w_before = { rating = w.rating, wins = w.wins, losses = w.losses, games = w.games }
        local l_before = { rating = l.rating, wins = l.wins, losses = l.losses, games = l.games }
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
            after_settlement(context, value, {
                { user_id = winner_id, username = username_in(value, winner_id), before = w_before, after = w, won = true },
                { user_id = loser_id, username = username_in(value, loser_id), before = l_before, after = l, won = false },
            })
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
            if changed and pcall(nk.storage_write, { ranked.set_object(value, version) }) and value.status == "disputed" then
                notify_disputed(context, value)
            end
            if type(mine) == "table" and mine.winner == winner then
                -- A retry of a claim that counted.
                local game = value.game or ""
                local r = read_rating(user_id, game)
                return nk.json_encode(level_fields(context, game, r.rating, { accepted = true, settled = value.status == "settled",
                    status = value.status, match_id = id, rating = r.rating }))
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
                notify_disputed(context, value)
                return nk.json_encode({ accepted = true, settled = false, status = "disputed", reason = value.reason, match_id = id })
            end
        else
            local result, w, l = settle_set(context, value, version, winner, "players")
            if result == true then
                signal_session(value)
                local mine_rating, their_rating = w, l
                if winner ~= user_id then
                    mine_rating, their_rating = l, w
                end
                local game = value.game or ""
                local answer = level_fields(context, game, mine_rating.rating, { accepted = true, settled = true,
                    status = "settled", match_id = id, rating = mine_rating.rating, opponent_rating = their_rating.rating })
                answer.opponent_level = level_of(context, game, their_rating.rating)
                answer.opponent_level_text = levels.format(answer.opponent_level)
                return nk.json_encode(answer)
            end
            -- "settled" or "changed": read the set again.
        end
    end
    error({ "the ranked set kept changing while the claim was recorded; retry", 10 })
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

local function get_rating(context, payload)
    local data = decode(payload)
    local game = tostring(data.game or "")
    if game == "" then
        error({ "game is required", 3 })
    end
    local record = read_rating(context.user_id, game)
    if context.user_id ~= nil and context.user_id ~= "" then
        local ok, written = pcall(sync_entry, context.user_id, context.username, game, record)
        if not ok then
            nk.logger_warn(string.format("ikemen ranked: cannot check the leaderboard entry of %s for %s: %s",
                context.user_id, game, tostring(written)))
        elseif written then
            nk.logger_info(string.format("ikemen ranked: leaderboard entry of %s for %s written (%d sets)", context.user_id, game, record.games))
        end
    end
    return nk.json_encode(level_fields(context, game, record.rating, record))
end

-- The characters of text, without control characters and surrounding
-- spaces, at most limit of them (as the lobby cuts names).
local function clean_text(value, limit)
    local text = tostring(value or "")
    local out = {}
    local count = 0
    local i = 1
    while i <= #text and count < limit do
        local c = string.byte(text, i)
        local size = 1
        if c >= 0xF0 then
            size = 4
        elseif c >= 0xE0 then
            size = 3
        elseif c >= 0xC0 then
            size = 2
        end
        if c >= 0x20 and c ~= 0x7F and (c < 0x80 or c >= 0xC0) then
            out[#out + 1] = string.sub(text, i, i + size - 1)
            count = count + 1
        end
        i = i + size
    end
    return (table.concat(out):gsub("^%s+", ""):gsub("%s+$", ""))
end

-- A player's online name: the account's display name, or its username until
-- the player chooses a name, cut to 16 characters.
local function online_name(user)
    local name = clean_text(user and user.display_name, 16)
    if name == "" then
        name = clean_text(user and user.username, 16)
    end
    return name
end

local function public_record(context, game, record, names)
    local metadata = type(record.metadata) == "table" and record.metadata or {}
    local elo = tonumber(metadata.rating) or (tonumber(record.score) and tonumber(record.score) / 1000) or config_for(game).initial_rating
    local out = {
        rank = tonumber(record.rank) or 0,
        user_id = record.owner_id,
        name = names[record.owner_id] or clean_text(record.username, 16),
        wins = tonumber(record.subscore) or tonumber(metadata.wins) or 0,
        losses = tonumber(metadata.losses) or 0,
    }
    local level = level_of(context, game, elo)
    out.level = level
    out.level_text = levels.format(level)
    return out
end

local function leaderboard(context, payload)
    local data = decode(payload)
    local game = tostring(data.game or "")
    if game == "" then
        error({ "game is required", 3 })
    end
    if #game > levels.GAME_ID_MAX then
        error({ string.format("a game id has at most %d bytes", levels.GAME_ID_MAX), 3 })
    end
    local limit = data.limit == nil and 20 or tonumber(data.limit)
    if limit == nil or limit ~= math.floor(limit) or limit < 1 or limit > 100 then
        error({ "limit must be 1 to 100", 3 })
    end
    local cursor = data.cursor ~= nil and tostring(data.cursor) or ""
    local out = { label = levels.label(context, game), records = {}, cursor = "" }
    local id = leaderboard_id(game)
    if #nk.leaderboards_get_id({ id }) == 0 then
        return (string.gsub(nk.json_encode(out), '"records":{}', '"records":[]'))
    end
    -- No owners for a call without a player's session: an empty list would
    -- list no records at all.
    local owners = nil
    if context.user_id ~= nil and context.user_id ~= "" then
        owners = { context.user_id }
    end
    local ok, records, owner_records, next_cursor = pcall(nk.leaderboard_records_list, id, owners, limit, cursor, 0)
    if not ok then
        if cursor ~= "" then
            error({ "cursor is not a cursor this list gave", 3 })
        end
        error(records)
    end
    local ids, seen = {}, {}
    local function want(r)
        if type(r) == "table" and type(r.owner_id) == "string" and not seen[r.owner_id] then
            seen[r.owner_id] = true
            ids[#ids + 1] = r.owner_id
        end
    end
    for _, r in ipairs(records or {}) do
        want(r)
    end
    for _, r in ipairs(owner_records or {}) do
        want(r)
    end
    local names = {}
    if #ids > 0 then
        local okUsers, users = pcall(nk.users_get_id, ids)
        if okUsers and type(users) == "table" then
            for _, u in ipairs(users) do
                names[u.user_id] = online_name(u)
            end
        end
    end
    for _, r in ipairs(records or {}) do
        out.records[#out.records + 1] = public_record(context, game, r, names)
    end
    out.cursor = next_cursor or ""
    if type(owner_records) == "table" and type(owner_records[1]) == "table" then
        out.mine = public_record(context, game, owner_records[1], names)
    end
    return (string.gsub(nk.json_encode(out), '"records":{}', '"records":[]'))
end

nk.register_rpc(submit_result, "ikemen_submit_result")
nk.register_rpc(get_rating, "ikemen_get_rating")
nk.register_rpc(leaderboard, "ikemen_leaderboard")

return {
    config_for = config_for,
    read_rating = read_rating,
    level_of = level_of,
    level_fields = level_fields,
    settle_set = settle_set,
    notify_disputed = notify_disputed,
}
