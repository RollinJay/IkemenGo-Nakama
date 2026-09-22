local nk = require("nakama")

-- IKEMEN GO ranked/unranked matchmaking.
--
-- Elo is read from Nakama storage, not trusted from the client ticket.
-- Game developers tune per-game ranked settings below.

local DEFAULTS = {
    initial_rating = 1000,
    k_factor = 32,
    elo_range = 100,
}

local GAME_CONFIG = {
    -- Example:
    -- ["my_game"] = { initial_rating = 1000, k_factor = 32, elo_range = 100 },
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

    if type(value) == "string" then
        local decoded = nk.json_decode(value)
        if type(decoded) == "table" and decoded.rating ~= nil then
            return tonumber(decoded.rating) or cfg.initial_rating
        end
    end

    return cfg.initial_rating
end

local function prop_string(entry, key, fallback)
    local value = entry.properties and entry.properties[key]
    if value == nil then
        return fallback
    end
    return tostring(value)
end

local function entry_key(entry)
    local mode = prop_string(entry, "mode", "unranked")
    local game = prop_string(entry, "game", "")
    local build = prop_string(entry, "build", "")
    local region = prop_string(entry, "region", "")
    local best_of = prop_string(entry, "ranked_best_of", "3")
    local switch_sides = prop_string(entry, "ranked_switch_sides", "false")
    local winner_keeps = prop_string(entry, "ranked_winner_keeps_selection", "false")
    if mode ~= "ranked" then
        best_of, switch_sides, winner_keeps = "0", "false", "false"
    end
    return table.concat({ game, build, mode, region, best_of, switch_sides, winner_keeps }, "\31")
end

local function numeric_property(entry, key, fallback)
    local value = entry.properties and entry.properties[key]
    local number = tonumber(value)
    if number == nil then
        return fallback
    end
    return number
end

local function process_bucket(entries, game)
    local groups = {}
    local used = {}

    for i, first in ipairs(entries) do
        if not used[i] then
            local first_mode = prop_string(first, "mode", "unranked")
            local best_index = nil
            local best_distance = nil

            if first_mode == "ranked" then
                local first_rating = get_rating(first.presence.user_id, game)
                local first_range = numeric_property(first, "elo_range", config_for(game).elo_range)
                for j, candidate in ipairs(entries) do
                    if i ~= j and not used[j] then
                        local candidate_rating = get_rating(candidate.presence.user_id, game)
                        local candidate_range = numeric_property(candidate, "elo_range", config_for(game).elo_range)
                        local distance = math.abs(first_rating - candidate_rating)
                        local allowed = math.max(first_range, candidate_range)
                        if distance <= allowed and (best_distance == nil or distance < best_distance) then
                            best_index = j
                            best_distance = distance
                        end
                    end
                end
            else
                for j = i + 1, #entries do
                    if not used[j] then
                        best_index = j
                        break
                    end
                end
            end

            if best_index ~= nil then
                used[i] = true
                used[best_index] = true
                table.insert(groups, { first, entries[best_index] })
            end
        end
    end
	return groups
end

local function matchmaker_processor(context, entries)
    local buckets = {}
    for _, entry in ipairs(entries) do
        local key = entry_key(entry)
        if buckets[key] == nil then
            buckets[key] = {}
        end
        table.insert(buckets[key], entry)
    end

    local groups = {}
    for _, bucket in pairs(buckets) do
        local game = prop_string(bucket[1], "game", "")
        local processed = process_bucket(bucket, game)
        for _, group in ipairs(processed) do
            if #group >= 2 then
                table.insert(groups, group)
            end
        end
    end
    return groups
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

nk.register_matchmaker_processor(matchmaker_processor)
nk.register_matchmaker_matched(matchmaker_matched)
