local nk = require("nakama")

-- IKEMEN GO Elo storage and result reconciliation.
--
-- This performs server-side Elo arithmetic and requires reciprocal result
-- claims before changing ratings. It is not a cryptographic anti-cheat system:
-- colluding clients can still submit matching false results.

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

local function read_rating(user_id, game)
    local cfg = config_for(game)
    local records = nk.storage_read({{
        collection = "ikemen_rating",
        key = rating_key(game),
        user_id = user_id,
    }})
    if #records == 0 then
        return { rating = cfg.initial_rating, wins = 0, losses = 0, games = 0 }
    end

    local value = records[1].value
    if type(value) == "table" then
        return {
            rating = tonumber(value.rating) or cfg.initial_rating,
            wins = tonumber(value.wins) or 0,
            losses = tonumber(value.losses) or 0,
            games = tonumber(value.games) or 0,
        }
    end

    return { rating = cfg.initial_rating, wins = 0, losses = 0, games = 0 }
end

local function write_rating(user_id, game, record)
    nk.storage_write({{
        collection = "ikemen_rating",
        key = rating_key(game),
        user_id = user_id,
        value = record,
        permission_read = 1,
        permission_write = 0,
    }})
end

local function submit_result(context, payload)
    local data = nk.json_decode(payload or "{}") or {}
    local match_id = tostring(data.match_id or "")
    local opponent_id = tostring(data.opponent_id or "")
    local game = tostring(data.game or "")
    local result = tonumber(data.result)
    local replay_hash = tostring(data.replay_hash or "")

    if match_id == "" or opponent_id == "" or game == "" or (result ~= 0 and result ~= 1) then
        error("invalid IKEMEN result submission")
    end
    if opponent_id == context.user_id then
        error("result opponent cannot be the submitting user")
    end

    local key = match_id .. ":" .. context.user_id

    local settlement_key = match_id .. ":settled"
    local settled = nk.storage_read({{
        collection = "ikemen_result",
        key = settlement_key,
        user_id = context.user_id,
    }})
    if #settled > 0 then
        local existing = settled[1].value
        if type(existing) == "table" then
            return nk.json_encode({
                accepted = true,
                settled = true,
                rating = read_rating(context.user_id, game).rating,
            })
        end
    end

    nk.storage_write({{
        collection = "ikemen_result",
        key = key,
        user_id = context.user_id,
        value = {
            match_id = match_id,
            opponent_id = opponent_id,
            game = game,
            result = result,
            replay_hash = replay_hash,
        },
        permission_read = 0,
        permission_write = 0,
    }})

    local other_key = match_id .. ":" .. opponent_id
    local other = nk.storage_read({{
        collection = "ikemen_result",
        key = other_key,
        user_id = opponent_id,
    }})
    if #other == 0 then
        return nk.json_encode({ accepted = true, settled = false })
    end

    local claim = other[1].value
    if type(claim) ~= "table" then
        return nk.json_encode({ accepted = true, settled = false })
    end
    if tostring(claim.opponent_id or "") ~= context.user_id then
        return nk.json_encode({ accepted = true, settled = false })
    end
    if tostring(claim.game or "") ~= game then
        return nk.json_encode({ accepted = true, settled = false })
    end
    if tonumber(claim.result) + result ~= 1 then
        return nk.json_encode({ accepted = true, settled = false, reason = "claims_disagree" })
    end
    if claim.replay_hash ~= "" and replay_hash ~= "" and claim.replay_hash ~= replay_hash then
        return nk.json_encode({ accepted = true, settled = false, reason = "replay_hash_disagree" })
    end

    local cfg = config_for(game)
    local me = read_rating(context.user_id, game)
    local them = read_rating(opponent_id, game)
    local expected_me = 1 / (1 + math.pow(10, (them.rating - me.rating) / cfg.rating_scale))
    local expected_them = 1 - expected_me
    me.rating = me.rating + cfg.k_factor * (result - expected_me)
    them.rating = them.rating + cfg.k_factor * ((1 - result) - expected_them)
    me.games = me.games + 1
    them.games = them.games + 1
    if result == 1 then
        me.wins = me.wins + 1
        them.losses = them.losses + 1
    else
        them.wins = them.wins + 1
        me.losses = me.losses + 1
    end

    write_rating(context.user_id, game, me)
    write_rating(opponent_id, game, them)

    nk.storage_write({{
        collection = "ikemen_result",
        key = settlement_key,
        user_id = context.user_id,
        value = {
            match_id = match_id,
            player_id = context.user_id,
            opponent_id = opponent_id,
            game = game,
        },
        permission_read = 0,
        permission_write = 0,
    }})

    return nk.json_encode({
        accepted = true,
        settled = true,
        rating = me.rating,
        opponent_rating = them.rating,
    })
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
