local nk = require("nakama")

-- IKEMEN GO ranked/unranked matchmaking.
--
-- Built on Nakama's own matchmaker (the Lua runtime has no matchmaker
-- processor/override hook; those are Go-runtime only). Compatibility buckets
-- come from the client's "+properties.<key>:<value>" query clauses. For ranked
-- tickets a realtime before-hook replaces the client-supplied Elo with the
-- stored rating and appends the allowed Elo window to the ticket query, so
-- the rating is never trusted from the client.

local DEFAULTS = {
    initial_rating = 1000,
    elo_range = 100,
    max_elo_range = 1000,
}

local GAME_CONFIG = {
    -- Example:
    -- ["my_game"] = { initial_rating = 1000, elo_range = 100, max_elo_range = 400 },
}

local function config_for(game)
    return GAME_CONFIG[game] or DEFAULTS
end

local function get_rating(user_id, game)
    local cfg = config_for(game)
    local records = nk.storage_read({{
        collection = "ikemen_rating",
        key = game,
        user_id = user_id,
    }})
    if #records == 0 then
        return cfg.initial_rating
    end
    local value = records[1].value
    if type(value) == "table" and value.rating ~= nil then
        return tonumber(value.rating) or cfg.initial_rating
    end
    return cfg.initial_rating
end

local function before_matchmaker_add(context, envelope)
    local add = envelope.matchmaker_add
    if type(add) ~= "table" then
        return envelope
    end
    add.string_properties = add.string_properties or {}
    add.numeric_properties = add.numeric_properties or {}
    if add.string_properties.mode ~= "ranked" then
        return envelope
    end

    local game = tostring(add.string_properties.game or "")
    local cfg = config_for(game)
    local rating = math.floor(get_rating(context.user_id, game) + 0.5)
    local range = math.floor(tonumber(add.numeric_properties.elo_range) or cfg.elo_range)
    if range < 0 then range = 0 end
    if range > cfg.max_elo_range then range = cfg.max_elo_range end

    -- Server-authoritative values; the client's copies are overwritten.
    add.numeric_properties.elo = rating
    add.numeric_properties.elo_range = range
    add.query = string.format("%s +properties.elo:>=%d +properties.elo:<=%d",
        tostring(add.query or ""), rating - range, rating + range)
    return envelope
end

local function matchmaker_matched(context, matched_users)
    if #matched_users ~= 2 then
        return nil
    end
    local props = matched_users[1].properties or {}
    return nk.match_create("ikemen_session", {
        mode = props.mode or "unranked",
        game = props.game or "",
        build = props.build or "",
        ranked_best_of = props.ranked_best_of or "3",
        ranked_switch_sides = props.ranked_switch_sides or "false",
        ranked_winner_keeps_selection = props.ranked_winner_keeps_selection or "false",
        expected_users = matched_users,
    })
end

nk.register_rt_before(before_matchmaker_add, "MatchmakerAdd")
nk.register_matchmaker_matched(matchmaker_matched)
