local nk = require("nakama")

-- IKEMEN GO ranked levels: the number players see for their ranked rating,
-- set by the server's creator.
--
-- Each player keeps an ordinary Elo rating per game (ikemen_rating.lua): it
-- starts at 1000, the matchmaker pairs players by it, and each ranked set
-- changes it. What players see is a level computed from it:
--
--   level = start * (top / start) ^ ((Elo - 1000) / 1000)
--
-- start is the level of a new player (Elo 1000), and top the level of a
-- player 1000 Elo above that: in a large community, the very top. The level
-- multiplies by the same factor for every step of Elo, so it is always
-- positive. It is rounded to a whole number, at least 1 and at most MAX
-- (9,000,000,000,000,000: up to there, the server's numbers hold every whole
-- number exactly). Without settings, the level is the Elo rating itself,
-- rounded, and its label is "Rating". The settings map every player's level
-- when it is shown: changing them changes everyone's level at once, and no
-- stored rating.
--
-- Settings: IKEMEN_LEVELS in Nakama's runtime.env, a JSON object:
--
--   {"default": {"start": 9001, "top": 1000000000, "label": "Power level"},
--    "games": {"<game id>": {"start": 100, "top": 10000, "label": "Rank"}}}
--
--   default  the levels of every game without an entry of its own; without
--            it, the plain Elo ratings labelled "Rating".
--   games    entries of their own, by game id (the game a server profile
--            names, as the game sends it in its tickets): 1 to GAME_ID_MAX
--            bytes, so that the game's leaderboard ID, ikemen_rating.<game
--            id>, fits in Nakama's 128.
--   An entry has start and top (both, or neither) and label, all optional:
--            start a number of at least 1, top a number more than start and
--            at most MAX, label a name for the number of 1 to LABEL_MAX
--            characters without control characters (surrounding spaces are
--            removed). A game's entry takes what it leaves out from default
--            (start and top together); default's label is "Rating" when it
--            gives none.
-- Any other field is an error. A value that is not valid is logged when the
-- server starts, and the server shows the plain Elo ratings, labelled
-- "Rating", for every game; a valid value is logged too.
--
-- Levels are texts as well as numbers: text() writes them with thousands
-- separators (9,001), on the server, so that every client shows the same
-- text.

local M = {}

M.MAX = 9000000000000000
M.DEFAULT_LABEL = "Rating"
M.LABEL_MAX = 32
M.GAME_ID_MAX = 114

-- From 2^52 on, a number is a whole number already (and adding 0.5 to it
-- would round to the next even one).
local WHOLE = 4503599627370496

-- The plain Elo ratings: an entry without start and top.
local PLAIN = { label = M.DEFAULT_LABEL }

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

-- Whether text has a control character: C0, DEL or C1 (U+0080 to U+009F,
-- encoded C2 80 to C2 9F).
local function has_control(text)
    for i = 1, #text do
        local b = string.byte(text, i)
        if b < 0x20 or b == 0x7F then
            return true
        end
        if b == 0xC2 then
            local c = string.byte(text, i + 1)
            if c ~= nil and c >= 0x80 and c <= 0x9F then
                return true
            end
        end
    end
    return false
end

local function number_text(v)
    if v == math.floor(v) and v > -WHOLE * 2 and v < WHOLE * 2 then
        return string.format("%d", v)
    end
    return tostring(v)
end

-- An entry of IKEMEN_LEVELS (where names it in messages), with what it
-- leaves out taken from base; or nil and what is wrong.
local function check_entry(where, entry, base)
    if type(entry) ~= "table" then
        return nil, where .. " must be an object"
    end
    for k in pairs(entry) do
        if k ~= "start" and k ~= "top" and k ~= "label" then
            return nil, string.format("%s has an unknown field %s (it has start, top and label)", where, tostring(k))
        end
    end
    local out = { label = M.DEFAULT_LABEL }
    if base ~= nil then
        out.start, out.top, out.label = base.start, base.top, base.label
    end
    if entry.start ~= nil or entry.top ~= nil then
        local start, top = entry.start, entry.top
        if type(start) ~= "number" or type(top) ~= "number" then
            return nil, where .. ": start and top go together, as numbers"
        end
        if start ~= start or start < 1 then
            return nil, where .. ": start must be at least 1"
        end
        if top ~= top or top <= start then
            return nil, where .. ": top must be more than start"
        end
        if top > M.MAX then
            return nil, where .. ": top must be at most 9000000000000000"
        end
        out.start, out.top = start, top
    end
    if entry.label ~= nil then
        if type(entry.label) ~= "string" then
            return nil, where .. ": label must be text"
        end
        local label = entry.label:gsub("^%s+", ""):gsub("%s+$", "")
        local n = characters(label)
        if n < 1 or n > M.LABEL_MAX or has_control(label) then
            return nil, string.format("%s: label must have 1 to %d characters, none of them a control character", where, M.LABEL_MAX)
        end
        out.label = label
    end
    return out
end

-- The settings of an IKEMEN_LEVELS value: {default = entry, games = {[game
-- id] = entry}}, an entry being {start, top, label} (start and top nil for
-- the plain Elo ratings); or nil and what is wrong. An empty or absent value
-- gives the plain Elo ratings.
function M.parse(text)
    if text == nil or not string.find(text, "%S") then
        return { default = PLAIN, games = {} }
    end
    local ok, value = pcall(nk.json_decode, text)
    if not ok or type(value) ~= "table" then
        return nil, "it is not a JSON object"
    end
    for k in pairs(value) do
        if k ~= "default" and k ~= "games" then
            return nil, string.format("unknown field %s (it has default and games)", tostring(k))
        end
    end
    local default = PLAIN
    if value.default ~= nil then
        local entry, err = check_entry("default", value.default, nil)
        if entry == nil then
            return nil, err
        end
        default = entry
    end
    local games = {}
    if value.games ~= nil then
        if type(value.games) ~= "table" then
            return nil, "games must be an object"
        end
        for id, entry in pairs(value.games) do
            if type(id) ~= "string" or id == "" or #id > M.GAME_ID_MAX then
                return nil, string.format("games: %s is not a game id (1 to %d bytes)", tostring(id), M.GAME_ID_MAX)
            end
            local checked, err = check_entry(string.format("games[%q]", id), entry, default)
            if checked == nil then
                return nil, err
            end
            games[id] = checked
        end
    end
    return { default = default, games = games }
end

-- The settings parsed last (each Lua runtime of the server keeps its own).
local cached_text, cached = nil, nil

-- The server's settings: IKEMEN_LEVELS of context.env, or the plain Elo
-- ratings when it is not valid.
function M.settings(context)
    local env = (context and context.env) or {}
    local text = env.IKEMEN_LEVELS or ""
    if text ~= cached_text then
        cached = M.parse(text) or { default = PLAIN, games = {} }
        cached_text = text
    end
    return cached
end

-- The entry that sets a game's levels.
function M.entry(context, game)
    local s = M.settings(context)
    return s.games[tostring(game or "")] or s.default
end

local function round(v)
    if v >= WHOLE or v <= -WHOLE then
        return v
    end
    return math.floor(v + 0.5)
end

-- The level of an Elo rating under entry.
function M.level_of(entry, elo)
    elo = tonumber(elo) or 1000
    if entry.start == nil then
        return round(elo)
    end
    local v = entry.start * math.pow(entry.top / entry.start, (elo - 1000) / 1000)
    if v ~= v then
        v = 1
    end
    v = round(v)
    if v < 1 then
        v = 1
    elseif v > M.MAX then
        v = M.MAX
    end
    return v
end

-- A whole number with thousands separators: 9,001; -1,239.
function M.format(n)
    local s = string.format("%d", n)
    local sign = ""
    if string.sub(s, 1, 1) == "-" then
        sign, s = "-", string.sub(s, 2)
    end
    local parts = {}
    local head = #s % 3
    if head > 0 then
        parts[1] = string.sub(s, 1, head)
    end
    for i = head + 1, #s, 3 do
        parts[#parts + 1] = string.sub(s, i, i + 2)
    end
    return sign .. table.concat(parts, ",")
end

-- A change of level with its sign: +1,239, -1,239, +0.
function M.change_text(d)
    if d >= 0 then
        return "+" .. M.format(d)
    end
    return M.format(d)
end

-- The level of a player of game whose Elo rating is elo.
function M.level(context, game, elo)
    return M.level_of(M.entry(context, game), elo)
end

-- The same as text: 9,001.
function M.text(context, game, elo)
    return M.format(M.level(context, game, elo))
end

-- The name of game's number: "Rating" without settings.
function M.label(context, game)
    return M.entry(context, game).label
end

-- Adds level, level_text and label to out (an RPC's answer) and returns it.
function M.fields(context, game, elo, out)
    local entry = M.entry(context, game)
    local level = M.level_of(entry, elo)
    out.level = level
    out.level_text = M.format(level)
    out.label = entry.label
    return out
end

local function describe_entry(entry)
    if entry.start == nil then
        return string.format("the plain Elo ratings, labelled %q", entry.label)
    end
    return string.format("start %s, top %s, labelled %q", number_text(entry.start), number_text(entry.top), entry.label)
end

-- What the settings are, for the server's log.
function M.describe(s)
    local ids = {}
    for id in pairs(s.games) do
        ids[#ids + 1] = id
    end
    table.sort(ids)
    local text = "levels are " .. describe_entry(s.default)
    for _, id in ipairs(ids) do
        text = text .. string.format("; game %q: %s", id, describe_entry(s.games[id]))
    end
    return text
end

-- Once, when the server starts: the settings in effect, or why the value
-- given is not valid.
nk.run_once(function(context)
    local text = ((context and context.env) or {}).IKEMEN_LEVELS
    local s, err = M.parse(text)
    if s == nil then
        nk.logger_error(string.format("ikemen levels: IKEMEN_LEVELS is not valid (%s): levels are the plain Elo ratings, labelled %q, for every game",
            err, M.DEFAULT_LABEL))
        return
    end
    local source = (text == nil or text == "") and " (IKEMEN_LEVELS is not set)" or ""
    nk.logger_info("ikemen levels: " .. M.describe(s) .. source)
end)

return M
