local nk = require("nakama")

-- Connection telemetry: which route the P2P session of two players runs
-- over (direct or through a relay), as their games report it with the
-- "link" command (src/nakama_watch.go reportLink). The lobby and the session
-- modules keep the latest report per pair and log one INFO line when a
-- pairing's route is first reported and each time it changes, so the
-- server's log counts how often matches play direct (nakama/README.md,
-- Connection telemetry).
--
-- Route names: direct-host, direct-vpn, direct-srflx, direct-prflx,
-- direct-predicted, direct-router, direct-ipv6, relay-udp, relay-ws. A
-- client of a later version may name other routes: any short name of
-- lowercase letters, digits and hyphens is kept as it is.

local M = {}

local ROUTE_MAX = 24
local UPGRADES_MAX = 1000

-- The route a client reported, or "" when it sent none or an invalid one
-- (a client before the telemetry sends none).
function M.route(value)
    if type(value) ~= "string" or #value == 0 or #value > ROUTE_MAX or value:find("[^a-z0-9%-]") then
        return ""
    end
    return value
end

-- How many times the session changed route after its first, 0..1000.
function M.upgrades(value)
    local n = tonumber(value)
    if n == nil or n ~= n then
        return 0
    end
    n = math.floor(n)
    if n < 0 then
        return 0
    end
    if n > UPGRADES_MAX then
        return UPGRADES_MAX
    end
    return n
end

-- "direct", "relay" or "" for a route name.
function M.kind(route)
    if route == nil or route == "" then
        return ""
    end
    if route:sub(1, 5) == "relay" then
        return "relay"
    end
    return "direct"
end

-- Names in a log line: no spaces, so the line splits on them.
local function word(value)
    value = tostring(value or "")
    value = value:gsub("[%s=]", "_")
    if value == "" then
        return "?"
    end
    return value
end

-- Notes the route of pairing `pairing` (a number or string naming it
-- within `where`) of p1 and p2 in `entry` (a table kept per pair) and logs
-- one line when the pairing's route is first known or changed:
--   ikemen <where>: pairing seq=<pairing> <p1>-<p2> route=<route> rtt=<ms> upgrades=<n> id=<match id>
-- Returns true when it logged.
function M.note(entry, where, match_id, pairing, p1, p2, route, rtt, upgrades)
    if route == "" then
        return false
    end
    if entry.logged_pairing == pairing and entry.logged_route == route then
        return false
    end
    entry.logged_pairing = pairing
    entry.logged_route = route
    nk.logger_info(string.format("ikemen %s: pairing seq=%s %s-%s route=%s rtt=%d upgrades=%d id=%s",
        where, word(pairing), word(p1), word(p2), route, math.floor(tonumber(rtt) or 0), upgrades or 0, word(match_id)))
    return true
end

return M
