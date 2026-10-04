local nk = require("nakama")

-- IKEMEN GO online names.
--
-- A player's online name is the account's display name. Lobbies show it with
-- a tag taken from the account ID, and the game's fight screen shows it in
-- place of the name of the character the player controls. Names need not be
-- unique: accounts are told apart by their IDs.
--
-- ikemen_account_name: {name} -> {display_name}. Sets the caller's display
-- name. A name has 2 to 16 characters, none of them a control character;
-- surrounding spaces are removed. Which characters a game accepts beyond
-- that is decided by the game (those its lifebar's name fonts can draw),
-- not here.
--
-- Errors (Nakama RPC error codes):
--   3 INVALID_ARGUMENT  the name breaks the rules above
--  13 INTERNAL          the account could not be updated
--
-- Nakama's own account update (PUT /v2/account) can set the display name
-- too: a before-hook holds it to the same rules. Before-hooks on
-- authentication keep a username Nakama would refuse from failing a sign-in.

local NAME_MIN = 2
local NAME_MAX = 16
-- Longer input cannot be a name, even with surrounding spaces. Refusing it
-- before trimming keeps the trim cheap: Lua's pattern matching takes time
-- quadratic in the length of a run of spaces that does not end the string.
local RAW_MAX = 256

-- The number of characters of s when it is valid UTF-8 without control
-- characters (C0, DEL, C1); nil otherwise.
local function name_length(s)
    local n, i, len = 0, 1, #s
    while i <= len do
        local c = s:byte(i)
        local size, lo, hi = 1, 0x80, 0xBF
        if c < 0x80 then
            size = 1
        elseif c >= 0xC2 and c <= 0xDF then
            size = 2
        elseif c >= 0xE0 and c <= 0xEF then
            size = 3
            if c == 0xE0 then
                lo = 0xA0
            elseif c == 0xED then
                hi = 0x9F
            end
        elseif c >= 0xF0 and c <= 0xF4 then
            size = 4
            if c == 0xF0 then
                lo = 0x90
            elseif c == 0xF4 then
                hi = 0x8F
            end
        else
            return nil
        end
        if i + size - 1 > len then
            return nil
        end
        for k = i + 1, i + size - 1 do
            local cc = s:byte(k)
            if k == i + 1 and (cc < lo or cc > hi) then
                return nil
            end
            if cc < 0x80 or cc > 0xBF then
                return nil
            end
        end
        if c < 0x20 or c == 0x7F or (c == 0xC2 and s:byte(i + 1) < 0xA0) then
            return nil
        end
        n = n + 1
        i = i + size
    end
    return n
end

-- The name without surrounding spaces when it follows the rules; raises the
-- RPC error otherwise.
local function checked_name(raw)
    local name = nil
    if type(raw) == "string" and #raw <= RAW_MAX then
        name = raw:gsub("^%s+", ""):gsub("%s+$", "")
    end
    local n = name and name_length(name)
    if n == nil or n < NAME_MIN or n > NAME_MAX then
        error({ "Names have 2 to 16 characters", 3 })
    end
    return name
end

local function set_name(context, payload)
    local ok, params = pcall(nk.json_decode, payload or "{}")
    local raw = nil
    if ok and type(params) == "table" then
        raw = params.name
    end
    local name = checked_name(raw)
    local updated, err = pcall(nk.account_update_id, context.user_id, nil, nil, name)
    if not updated then
        nk.logger_error(("ikemen_account_name: %s"):format(tostring(err)))
        error({ "The name could not be changed", 13 })
    end
    return nk.json_encode({ display_name = name })
end

nk.register_rpc(set_name, "ikemen_account_name")

-- PUT /v2/account: a new display name follows the same rules. Other fields
-- (username, avatar, language, location, time zone) pass unchanged.
local function before_update_account(context, payload)
    if type(payload) == "table" and payload.display_name ~= nil then
        payload.display_name = checked_name(payload.display_name)
    end
    return payload
end

nk.register_req_before(before_update_account, "UpdateAccount")

-- Authentication names a new account with the request's username (an
-- existing account keeps its name). Lobbies show a username only until the
-- player chooses an online name. A username Nakama would refuse is left out,
-- so that Nakama generates one instead of refusing the sign-in: one taken by
-- an account, over 128 bytes, with a control character, or with "[]", which
-- Nakama 3.41's username check also refuses. (Nakama accepts spaces.)
--
-- An email sign-in without an email address signs in to an existing account
-- by username and password; its username is left alone.
local function before_authenticate(id, payload)
    if type(payload) ~= "table" or type(payload.username) ~= "string" or payload.username == "" then
        return payload
    end
    if id == "AuthenticateEmail" and (type(payload.account) ~= "table" or (payload.account.email or "") == "") then
        return payload
    end
    local name = payload.username
    local drop = #name > 128 or name:find("%c") ~= nil or name:find("[]", 1, true) ~= nil
    if not drop then
        local rows = nk.sql_query("SELECT 1 FROM users WHERE username = $1 LIMIT 1", { name })
        drop = #rows > 0
    end
    if drop then
        payload.username = nil
    end
    return payload
end

for _, id in ipairs({ "AuthenticateApple", "AuthenticateCustom", "AuthenticateDevice", "AuthenticateEmail",
    "AuthenticateFacebook", "AuthenticateFacebookInstantGame", "AuthenticateGameCenter", "AuthenticateGoogle",
    "AuthenticateSteam" }) do
    nk.register_req_before(function(context, payload)
        return before_authenticate(id, payload)
    end, id)
end
