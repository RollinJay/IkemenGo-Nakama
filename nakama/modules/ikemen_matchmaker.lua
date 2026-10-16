local nk = require("nakama")
local ranked = require("ikemen_ranked")
local rating = require("ikemen_rating")

-- IKEMEN GO ranked/unranked matchmaking.
--
-- Built on Nakama's own matchmaker (the Lua runtime has no matchmaker
-- processor/override hook; those are Go-runtime only). Compatibility buckets
-- come from the client's "+properties.<key>:<value>" query clauses. For ranked
-- tickets a realtime before-hook replaces the client-supplied Elo with the
-- stored rating (ikemen_rating.lua, which also sets the rating of a player
-- who has none yet) and appends the allowed Elo window to the ticket query,
-- so the rating is never trusted from the client.

-- The Elo window of ranked tickets. The ratings themselves, and the rating
-- a new player starts with, are ikemen_rating.lua's.
local DEFAULTS = {
    elo_range = 100,
    max_elo_range = 1000,
}

local GAME_CONFIG = {
    -- Example:
    -- ["my_game"] = { elo_range = 100, max_elo_range = 400 },
}

local function config_for(game)
    return GAME_CONFIG[game] or DEFAULTS
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
    local elo = math.floor(rating.read_rating(context.user_id, game).rating + 0.5)
    local range = math.floor(tonumber(add.numeric_properties.elo_range) or cfg.elo_range)
    if range < 0 then range = 0 end
    if range > cfg.max_elo_range then range = cfg.max_elo_range end

    -- Server-authoritative values; the client's copies are overwritten.
    add.numeric_properties.elo = elo
    add.numeric_properties.elo_range = range
    add.query = string.format("%s +properties.elo:>=%d +properties.elo:<=%d",
        tostring(add.query or ""), elo - range, elo + range)
    return envelope
end

-- Makes the session match of a matched pair. A ranked session's first set
-- is recorded here (ikemen_ranked.lua), with the players in the matchmaker's
-- order, which every player receives too: the first hosts and starts on side
-- 1. Results are accepted only for recorded sets.
local function matchmaker_matched(context, matched_users)
    if #matched_users ~= 2 then
        return nil
    end
    local props = matched_users[1].properties or {}
    local mode = props.mode or "unranked"
    if mode == "ranked" then
        -- A ranked session needs two ranked tickets for the same game, build
        -- and set length (each ticket's query asks for that; a client's own
        -- query is not trusted to).
        local other = matched_users[2].properties or {}
        for _, key in ipairs({ "mode", "game", "build", "ranked_best_of" }) do
            if tostring(other[key] or "") ~= tostring(props[key] or "") then
                nk.logger_warn(string.format("ikemen matchmaker: tickets differ in %s (%s, %s): the session is unranked",
                    key, tostring(props[key]), tostring(other[key])))
                mode = "unranked"
                break
            end
        end
    end
    local match_id = nk.match_create("ikemen_session", {
        mode = mode,
        game = props.game or "",
        build = props.build or "",
        ranked_best_of = props.ranked_best_of or "3",
        ranked_switch_sides = props.ranked_switch_sides or "false",
        ranked_winner_keeps_selection = props.ranked_winner_keeps_selection or "false",
        expected_users = matched_users,
    })
    if mode == "ranked" then
        local players = {}
        for i, user in ipairs(matched_users) do
            local presence = user.presence or {}
            players[i] = { user_id = presence.user_id or "", username = presence.username or "" }
        end
        ranked.create_set(match_id, 1, {
            game = props.game or "",
            build = props.build or "",
            best_of = props.ranked_best_of,
            players = players,
        })
    end
    return match_id
end

nk.register_rt_before(before_matchmaker_add, "MatchmakerAdd")
nk.register_matchmaker_matched(matchmaker_matched)
