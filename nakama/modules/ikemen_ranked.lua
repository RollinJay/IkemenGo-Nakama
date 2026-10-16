local nk = require("nakama")

-- IKEMEN GO ranked sets: the server's record of each ranked set, the
-- players' replays it keeps for moderators, and the match replay file a kept
-- replay becomes.
--
-- A ranked set is a best-of-N series between two players, played in a
-- ranked session: an ikemen_session match that the matchmaker made
-- (ikemen_matchmaker.lua). The matchmaker records the session's first set
-- when it makes the session. When both players choose "Play Another Set"
-- after a set, the session records the next one.
--
-- Storage, all of it system-owned (players can neither read nor write it):
--   ikemen_ranked_sets/<set id>                 the set's record, below
--   ikemen_ranked_set_index/<order>:<set id>    the sets, newest first (order
--                                               counts down from 9999999999999
--                                               with the time the set was
--                                               recorded, in milliseconds)
--   ikemen_ranked_replays/<set id>:<user id>    a player's replay stream of the set
--   ikemen_ranked_replay_expiry/<time>:<hash>   when each kept replay expires
-- A set id is the session's match id for the session's first set, and
-- "<match id>#<n>" for its n-th set.
--
-- A set record holds: match_id (the set id), session (the match id), set
-- (its number in the session), game, build, best_of, players (user_id and
-- username, in the matchmaker's order: the first player hosts the netplay
-- session and starts on side 1), created and ended (Unix time in seconds;
-- ended is when the session, or for an earlier set the set, ended), status,
-- reason, claims (by user id: user_id, winner, at), left_early (user ids),
-- played (true once a match of the set has started), replays (by user id:
-- user_id, bytes, complete), winner, settled_by ("players" or
-- "moderator"), decided (when the status last changed), note and, once the
-- set has settled, results (by user id: won, label, rating_before, rating,
-- level_before, level; ikemen_rating.lua).
--
-- Status:
--   open      the set is being played, or waits for its players' claims
--   settled   both claims named the same winner, or a moderator settled it:
--             the ratings changed, once (ikemen_rating.lua)
--   disputed  the claims named different winners (reason claims_disagree), or
--             a claim did not come in time (claim_timeout): one player
--             claimed, or nobody did after a player left a set whose
--             matches had begun
--   void      no result: nobody claimed in time, and no match was played or
--             nobody left early (no_claims), or a moderator voided it
--             (moderator)
--
-- Settings (Nakama's runtime.env):
--   IKEMEN_RANKED_REPLAY_DAYS       days a replay is kept (default 30; 0 keeps none)
--   IKEMEN_RANKED_REPLAY_MAX_BYTES  most bytes kept per player and set (default 16 MiB,
--                                   about 50 minutes of play; the session also keeps
--                                   them no faster than play makes them, see
--                                   ikemen_session.lua)
--   IKEMEN_RANKED_CLAIM_MINUTES     minutes the second claim may take after the
--                                   session ends (default 15)

local M = {}

M.SETS = "ikemen_ranked_sets"
M.INDEX = "ikemen_ranked_set_index"
M.REPLAYS = "ikemen_ranked_replays"
M.EXPIRY = "ikemen_ranked_replay_expiry"

-- The owner of system-owned storage objects.
M.SYSTEM_USER = "00000000-0000-0000-0000-000000000000"

-- A page of the system-owned objects of collection, and the cursor of the
-- next page. Nakama lets a player write objects of their own into any
-- collection (unless a before-hook refuses it, as Nakama Matchmaker's
-- community module does): listing without an owner would return those too,
-- so the lists name the system as the owner, and leave out any object of
-- another owner.
function M.list_system(collection, limit, cursor)
    local objects, next_cursor = nk.storage_list(M.SYSTEM_USER, collection, limit, cursor or "")
    local out = {}
    for _, o in ipairs(objects or {}) do
        if o.user_id == nil or o.user_id == "" or o.user_id == M.SYSTEM_USER then
            out[#out + 1] = o
        end
    end
    return out, next_cursor or ""
end

-- An open set whose session never reported its end (the server stopped
-- meanwhile) is timed out this long after it was made.
local OPEN_HOURS = 24

local DEFAULT_REPLAY_DAYS = 30
local DEFAULT_REPLAY_MAX_BYTES = 16 * 1024 * 1024
local DEFAULT_CLAIM_MINUTES = 15

local function setting(context, name, default)
    local env = (context and context.env) or {}
    local n = tonumber(env[name])
    if n == nil then
        return default
    end
    return n
end

function M.replay_days(context)
    return setting(context, "IKEMEN_RANKED_REPLAY_DAYS", DEFAULT_REPLAY_DAYS)
end

function M.replay_max_bytes(context)
    return math.max(0, math.floor(setting(context, "IKEMEN_RANKED_REPLAY_MAX_BYTES", DEFAULT_REPLAY_MAX_BYTES)))
end

function M.claim_minutes(context)
    return math.max(0, setting(context, "IKEMEN_RANKED_CLAIM_MINUTES", DEFAULT_CLAIM_MINUTES))
end

function M.now()
    return math.floor(nk.time() / 1000)
end

-- The id of the set numbered set (1 for the session's first) of the ranked
-- session match_id. An id that already names a set is returned as it is.
function M.set_id(match_id, set)
    match_id = tostring(match_id or "")
    if string.find(match_id, "#", 1, true) then
        return match_id
    end
    set = math.floor(tonumber(set) or 1)
    if set <= 1 then
        return match_id
    end
    return match_id .. "#" .. set
end

-- As the client's normalizeRankedBestOf: odd, 1 to 99, 3 by default.
function M.normalize_best_of(value)
    local n = math.floor(tonumber(value) or 3)
    if n < 1 then
        n = 3
    end
    if n % 2 == 0 then
        n = n + 1
    end
    if n > 99 then
        n = 99
    end
    return n
end

-- Records the set numbered set of the session match_id: info has game,
-- build, best_of and players ({user_id, username}, matchmaker order).
function M.create_set(match_id, set, info)
    local id = M.set_id(match_id, set)
    local value = {
        match_id = id,
        session = match_id,
        set = set,
        game = tostring(info.game or ""),
        build = tostring(info.build or ""),
        best_of = M.normalize_best_of(info.best_of),
        players = info.players or {},
        created = M.now(),
        status = "open",
        reason = "",
        claims = {},
        left_early = {},
        replays = {},
        note = "",
    }
    local ok, err = pcall(nk.storage_write, {
        {
            collection = M.SETS,
            key = id,
            value = value,
            version = "*",
            permission_read = 0,
            permission_write = 0,
        },
        {
            collection = M.INDEX,
            key = string.format("%013d:%s", math.max(0, 9999999999999 - math.floor(nk.time())), id),
            value = { match_id = id },
            permission_read = 0,
            permission_write = 0,
        },
    })
    if not ok then
        nk.logger_error(string.format("ikemen ranked: cannot record set %s: %s", id, tostring(err)))
        return nil
    end
    nk.logger_info(string.format("ikemen ranked: set %s recorded (%s, best of %d)", id, value.game, value.best_of))
    return value
end

-- Fills in the tables a stored record lacks (storage drops empty ones).
function M.normalize(value)
    value.claims = type(value.claims) == "table" and value.claims or {}
    value.left_early = type(value.left_early) == "table" and value.left_early or {}
    value.replays = type(value.replays) == "table" and value.replays or {}
    value.players = type(value.players) == "table" and value.players or {}
    value.results = type(value.results) == "table" and value.results or {}
    value.note = tostring(value.note or "")
    value.reason = tostring(value.reason or "")
    return value
end

-- The set's record and its storage version, or nil.
function M.read_set(id)
    if type(id) ~= "string" or id == "" or #id > 128 then
        return nil
    end
    local records = nk.storage_read({{ collection = M.SETS, key = id }})
    if #records == 0 or type(records[1].value) ~= "table" then
        return nil
    end
    return M.normalize(records[1].value), records[1].version
end

-- The storage object that writes value over the version read.
function M.set_object(value, version)
    return {
        collection = M.SETS,
        key = value.match_id,
        value = value,
        version = version,
        permission_read = 0,
        permission_write = 0,
    }
end

-- Changes a set's record: fn(value) changes it and returns true to write it.
-- Another writer between the read and the write makes it start again.
-- Returns the record as left (nil when there is none).
function M.update_set(id, fn)
    for _ = 1, 8 do
        local value, version = M.read_set(id)
        if value == nil then
            return nil
        end
        if not fn(value) then
            return value
        end
        if pcall(nk.storage_write, { M.set_object(value, version) }) then
            return value
        end
    end
    nk.logger_warn("ikemen ranked: set " .. tostring(id) .. " kept changing; an update was dropped")
    return nil
end

function M.player_index(value, user_id)
    for i, p in ipairs(value.players or {}) do
        if type(p) == "table" and p.user_id == user_id then
            return i
        end
    end
    return nil
end

local function count(t)
    local n = 0
    for _ in pairs(t or {}) do
        n = n + 1
    end
    return n
end
M.count = count

-- Times an open set out once its claims are overdue (the check is lazy: it
-- runs whenever a set is read to decide on it or to list it). Returns true
-- when it changed value.
function M.timed_out(context, value, now)
    if value.status ~= "open" then
        return false
    end
    now = now or M.now()
    local deadline
    if tonumber(value.ended) ~= nil then
        deadline = tonumber(value.ended) + M.claim_minutes(context) * 60
    else
        deadline = (tonumber(value.created) or now) + OPEN_HOURS * 3600
    end
    if now < deadline then
        return false
    end
    if count(value.claims) == 0 and (#value.left_early == 0 or value.played ~= true) then
        value.status, value.reason = "void", "no_claims"
    else
        value.status, value.reason = "disputed", "claim_timeout"
    end
    value.decided = now
    return true
end

-- The record as the moderator RPCs return it: claims, left_early and
-- replays as lists, in the players' order, and each player's result (a
-- settled set) with the player.
function M.public_set(value)
    local players, claims, replays, left = {}, {}, {}, {}
    for _, p in ipairs(value.players) do
        players[#players + 1] = { user_id = p.user_id, username = p.username or "" }
        local kept = value.results[p.user_id]
        if type(kept) == "table" then
            players[#players].result = {
                won = kept.won == true,
                label = type(kept.label) == "string" and kept.label or nil,
                rating_before = tonumber(kept.rating_before),
                rating = tonumber(kept.rating),
                level_before = tonumber(kept.level_before),
                level = tonumber(kept.level),
            }
        end
        local c = value.claims[p.user_id]
        if type(c) == "table" then
            claims[#claims + 1] = { user_id = p.user_id, winner = c.winner, at = c.at }
        end
        local r = value.replays[p.user_id]
        if type(r) == "table" then
            replays[#replays + 1] = { user_id = p.user_id, bytes = tonumber(r.bytes) or 0, complete = r.complete == true }
        end
    end
    for _, uid in ipairs(value.left_early) do
        left[#left + 1] = uid
    end
    return {
        match_id = value.match_id,
        session = value.session or value.match_id,
        set = tonumber(value.set) or 1,
        game = value.game or "",
        build = value.build or "",
        best_of = tonumber(value.best_of) or 3,
        status = value.status,
        reason = value.reason or "",
        players = players,
        claims = claims,
        left_early = left,
        replays = replays,
        played = value.played == true,
        created = tonumber(value.created) or 0,
        ended = tonumber(value.ended),
        decided = tonumber(value.decided),
        winner = value.winner,
        settled_by = value.settled_by,
        note = value.note or "",
    }
end

-- nk.json_encode writes an empty table as {}: these keys are lists.
local LIST_KEYS = { "sets", "players", "claims", "left_early", "replays" }

function M.encode(value)
    local text = nk.json_encode(value)
    for _, key in ipairs(LIST_KEYS) do
        text = string.gsub(text, '"' .. key .. '":{}', '"' .. key .. '":[]')
    end
    return text
end

-- Why match_id has no ranked set: an RPC error ({message, code}).
function M.no_set_error(match_id, set)
    local ok, match = pcall(nk.match_get, match_id)
    if ok and type(match) == "table" then
        local okLabel, label = pcall(nk.json_decode, match.label or "")
        if okLabel and type(label) == "table" then
            if label.kind == "ikemen-session" and label.mode ~= "ranked" then
                return { string.format("match %s is an unranked session: only ranked sessions are rated", match_id), 9 }
            end
            if label.kind == "ikemen-lobby" then
                return { string.format("match %s is a lobby: lobby matches are not rated", match_id), 9 }
            end
            if label.kind == "ikemen-session" and (tonumber(set) or 1) > 1 then
                return { string.format("ranked session %s has no set %d", match_id, tonumber(set)), 5 }
            end
            if label.kind == "ikemen-session" then
                return { string.format("ranked session %s has no recorded set (the server could not record it, or "
                    .. "made the session before it kept ranked sets)", match_id), 5 }
            end
        end
        return { string.format("match %s is not a ranked IKEMEN GO session", match_id), 9 }
    end
    return { string.format("no ranked set is recorded for match %s: the server did not make a ranked session with this id", match_id), 5 }
end

-- ---------------------------------------------------------------------------
-- Replays
--
-- A kept replay is the replay stream one player's game published during the
-- set (src/replay_stream.go): for each rollback session (a match, or a Turns
-- match's stretch between character changes), the header, the chunks of
-- inputs and the end or the reset, as the raw messages. A replay is complete
-- when the set was decided, every stream ended, the player did not leave
-- before the set was decided, and nothing was left out for the byte limit.

local function replay_key(set_id, user_id)
    return set_id .. ":" .. user_id
end

-- Stores a player's replay of a set and lists it on the set's record. buf
-- has streams ({header, chunks, final, reset, at, frames: the frames the
-- chunks hold}), bytes and truncated.
function M.store_replay(context, set_id, user_id, username, buf, complete)
    if M.replay_days(context) <= 0 or #(buf.streams or {}) == 0 then
        return
    end
    local key = replay_key(set_id, user_id)
    local now = M.now()
    local expires = now + math.floor(M.replay_days(context) * 86400)
    local value = {
        match_id = set_id,
        user_id = user_id,
        username = username or "",
        complete = complete == true and buf.truncated ~= true,
        truncated = buf.truncated == true,
        bytes = buf.bytes or 0,
        streams = buf.streams,
        stored = now,
        expires = expires,
    }
    -- A player who left and came back during the set, or whose stream began
    -- after the set was decided, has a replay of the set already: this one
    -- continues it, within the same byte limit. Together they are complete
    -- when both parts are.
    local old = nk.storage_read({{ collection = M.REPLAYS, key = key }})
    if #old > 0 and type(old[1].value) == "table" and type(old[1].value.streams) == "table" then
        local prev = old[1].value
        local streams = {}
        local bytes = tonumber(prev.bytes) or 0
        local max = M.replay_max_bytes(context)
        for _, s in ipairs(prev.streams) do
            streams[#streams + 1] = s
        end
        local dropped = false
        for _, s in ipairs(value.streams) do
            local size = #(s.header or "") + #(s.final or "")
            for _, c in ipairs(s.chunks or {}) do
                size = size + #c
            end
            if dropped or bytes + size > max then
                dropped = true
            else
                streams[#streams + 1] = s
                bytes = bytes + size
            end
        end
        value.streams = streams
        value.bytes = bytes
        value.truncated = value.truncated or dropped or prev.truncated == true
        value.complete = value.complete and not value.truncated and prev.complete == true
    end
    local index = string.format("%012d:%s", expires, string.sub(nk.sha256_hash(key), 1, 32))
    local ok, err = pcall(nk.storage_write, {
        { collection = M.REPLAYS, key = key, value = value, permission_read = 0, permission_write = 0 },
        { collection = M.EXPIRY, key = index, value = { key = key, set = set_id, user_id = user_id },
          permission_read = 0, permission_write = 0 },
    })
    if not ok then
        nk.logger_error(string.format("ikemen ranked: cannot store the replay %s: %s", key, tostring(err)))
        return
    end
    M.update_set(set_id, function(v)
        v.replays[user_id] = { user_id = user_id, bytes = value.bytes, complete = value.complete }
        return true
    end)
    nk.logger_info(string.format("ikemen ranked: replay %s stored (%d streams, %d bytes, %s)", key,
        #value.streams, value.bytes, value.complete and "complete" or "incomplete"))
    M.sweep(context, 20)
end

-- Removes up to limit replays whose time is up (oldest first).
function M.sweep(context, limit)
    local now = M.now()
    local ok, objects = pcall(M.list_system, M.EXPIRY, limit or 20, "")
    if not ok then
        nk.logger_warn("ikemen ranked: cannot list the replays' expiry: " .. tostring(objects))
        return
    end
    for _, o in ipairs(objects) do
        local at = tonumber(string.match(o.key or "", "^(%d+):"))
        if at == nil or at > now then
            break
        end
        local entry = type(o.value) == "table" and o.value or {}
        local deletes = {{ collection = M.EXPIRY, key = o.key }}
        if type(entry.key) == "string" then
            local r = nk.storage_read({{ collection = M.REPLAYS, key = entry.key }})
            -- A replay stored again later (a player who came back) has a later
            -- entry of its own.
            if #r > 0 and (tonumber(type(r[1].value) == "table" and r[1].value.expires) or 0) <= now then
                deletes[#deletes + 1] = { collection = M.REPLAYS, key = entry.key }
                if type(entry.set) == "string" and type(entry.user_id) == "string" then
                    M.update_set(entry.set, function(v)
                        if v.replays[entry.user_id] == nil then
                            return false
                        end
                        v.replays[entry.user_id] = nil
                        return true
                    end)
                end
                nk.logger_info("ikemen ranked: replay " .. entry.key .. " expired and was removed")
            end
        end
        pcall(nk.storage_delete, deletes)
    end
end

-- The kept replay of a player for a set, or nil and why. An expired replay
-- is removed.
function M.read_replay(context, set_id, user_id)
    local key = replay_key(set_id, user_id)
    local r = nk.storage_read({{ collection = M.REPLAYS, key = key }})
    if #r == 0 or type(r[1].value) ~= "table" then
        return nil, "none"
    end
    local value = r[1].value
    if (tonumber(value.expires) or 0) <= M.now() then
        M.sweep(context, 100)
        pcall(nk.storage_delete, {{ collection = M.REPLAYS, key = key }})
        M.update_set(set_id, function(v)
            if v.replays[user_id] == nil then
                return false
            end
            v.replays[user_id] = nil
            return true
        end)
        return nil, "expired"
    end
    return value
end

local function decode(text)
    if type(text) ~= "string" or text == "" then
        return nil
    end
    local ok, v = pcall(nk.json_decode, text)
    if ok and type(v) == "table" then
        return v
    end
    return nil
end

-- table.concat(list, sep) for a list of any length: table.concat puts every
-- piece on the Lua registry, whose size Nakama limits
-- (runtime.lua_registry_size, 512 by default), so the pieces are joined a
-- hundred at a time.
function M.join(list, sep)
    while #list > 100 do
        local groups = {}
        for i = 1, #list, 100 do
            groups[#groups + 1] = table.concat(list, sep, i, math.min(i + 99, #list))
        end
        list = groups
    end
    return table.concat(list, sep)
end

-- The match replay file of a kept replay (IKEMEN GO's format, see
-- src/replay_match.go): JSON lines, with a match record for each match of
-- the set, followed by the match's sessions, each with its header, its
-- inputs and its end. A Turns match has a session per character change
-- (segments 1, 2... of its streams). The game reads the file as it is, or
-- gzip-compressed. Returns the text and the number of matches.
function M.replay_file(replay)
    local lines = {}
    local matches = 0
    for _, s in ipairs(replay.streams or {}) do
        local header = decode(s.header)
        if header ~= nil then
            local final = decode(s.final)
            if (tonumber(header.segment) or 0) == 0 or matches == 0 then
                matches = matches + 1
                lines[#lines + 1] = nk.json_encode({
                    type = "match",
                    format = "ikemen-match-replay",
                    version = 1,
                    engine = tostring(header.engine or ""),
                    date = os.date("!%Y-%m-%dT%H:%M:%SZ", tonumber(s.at) or tonumber(replay.stored) or 0),
                    info = header.match_info,
                    content = final and final.content or nil,
                })
            end
            lines[#lines + 1] = '{"type":"session","header":' .. s.header .. '}'
            for _, chunk in ipairs(s.chunks or {}) do
                lines[#lines + 1] = '{"type":"frames",' .. string.sub(chunk, 2)
            end
            local last = final and tonumber(final.final_frame)
            if last == nil and type(s.reset) == "string" then
                -- A rollback past published frames withdrew the stream: the
                -- frames before it are the match's. (string.match returns
                -- nothing for another reason, such as byte_limit.)
                local frame = string.match(s.reset, "rollback_before_published_frame:(%d+)")
                last = frame and tonumber(frame)
            end
            -- Never past the frames kept (the game would wait for the rest);
            -- a stream without an end ends with its frames.
            if last ~= nil and tonumber(s.frames) ~= nil and last > tonumber(s.frames) then
                last = tonumber(s.frames)
            end
            if last ~= nil then
                lines[#lines + 1] = string.format('{"type":"end","final_frame":%d}', last)
            end
        end
    end
    return M.join(lines, "\n") .. "\n", matches
end

-- Keeps letters, digits, spaces, dashes, underscores and dots of a name in
-- a file name, at most 24 characters.
local function file_part(name)
    local s = string.gsub(tostring(name or ""), "[^%w%s%-_%.]", "_")
    s = string.gsub(s, "%s+", " ")
    s = string.sub(s, 1, 24)
    if s == "" then
        s = "player"
    end
    return s
end

-- A file name for a set's replay, as the game names its own files: the date,
-- the two players, then the set and whose replay it is.
function M.replay_file_name(set, replay)
    local names = {}
    for i, p in ipairs(set.players or {}) do
        names[i] = file_part(p.username)
    end
    local short = string.sub(tostring(set.session or set.match_id or ""), 1, 8)
    local which = "ranked " .. short
    if (tonumber(set.set) or 1) > 1 then
        which = which .. " set " .. tostring(set.set)
    end
    return string.format("%s %s vs %s (%s, %s).replay",
        os.date("!%Y-%m-%d_%Hh%Mm%Ss", tonumber(set.created) or 0),
        names[1] or "player", names[2] or "player", which, file_part(replay.username))
end

return M
