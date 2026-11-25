local nk = require("nakama")

-- Connection telemetry: which route the P2P session of two players runs
-- over (direct or through a relay), as their games report it with the
-- "link" command (src/nakama_watch.go reportLink). Each game picks its route
-- on its own, so the two players' views can differ for a while (one side
-- moved off the relay, the other not yet). The lobby and the session modules
-- keep the latest report of each player (combine), and the pair's route is
-- the lower of the two: relayed when either side is. They log one INFO line
-- when a pairing's route is first known and each time it changes, at most
-- LOG_MAX lines a pairing, so the server's log counts how often matches play
-- direct (nakama/README.md, Connection telemetry).
--
-- Route names: direct-host, direct-vpn, direct-srflx, direct-prflx,
-- direct-predicted, direct-router, direct-ipv6, relay-udp, relay-ws. A
-- client of a later version may name other routes: any short name of
-- lowercase letters, digits and hyphens is kept as it is.

local M = {}

local ROUTE_MAX = 24
local UPGRADES_MAX = 1000
-- Log lines a pairing writes at most: a route that keeps flapping does not
-- fill the log.
local LOG_MAX = 12

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

-- The rank of a route: 1 the websocket relay, 2 the UDP relay, 3 direct,
-- 0 none.
local function rank(route)
    if route == nil or route == "" then
        return 0
    end
    if route == "relay-ws" then
        return 1
    end
    if route:sub(1, 5) == "relay" then
        return 2
    end
    return 3
end

-- Records `sender`'s report of `route` in `entry` (a table kept per pair) and
-- returns the pair's route: the lower-ranked of the two players' latest
-- reports, or, when both rank the same, the report of `first` (the pair's
-- lesser user id), so the pair's route does not flip with whoever reported
-- last. Reports of an earlier pairing (`pairing`) are dropped. An empty
-- route keeps what the pair had.
function M.combine(entry, pairing, sender, first, route)
    if entry.routes == nil or entry.routes_pairing ~= pairing then
        entry.routes = {}
        entry.routes_pairing = pairing
    end
    if route ~= "" then
        entry.routes[sender] = route
    end
    local best, best_from = "", nil
    for from, r in pairs(entry.routes) do
        local better = best == "" or rank(r) < rank(best) or (rank(r) == rank(best) and from == first)
        if better then
            best, best_from = r, from
        end
    end
    return best
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
-- within `where`) of p1 and p2 in `entry` (a table kept per pair; route is
-- the pair's, from combine) and logs one line when the pairing's route is
-- first known or changed, up to LOG_MAX lines a pairing:
--   ikemen <where>: pairing seq=<pairing> <p1>-<p2> route=<route> rtt=<ms> upgrades=<n> id=<match id>
-- Returns true when it logged.
function M.note(entry, where, match_id, pairing, p1, p2, route, rtt, upgrades)
    if route == "" then
        return false
    end
    if entry.logged_pairing ~= pairing then
        entry.logged_pairing = pairing
        entry.logged_route = nil
        entry.logged_count = 0
    end
    if entry.logged_route == route then
        return false
    end
    entry.logged_route = route
    entry.logged_count = (entry.logged_count or 0) + 1
    if entry.logged_count > LOG_MAX + 1 then
        return false
    end
    if entry.logged_count == LOG_MAX + 1 then
        nk.logger_info(string.format("ikemen %s: pairing seq=%s %s-%s route keeps changing; no more lines for it id=%s",
            where, word(pairing), word(p1), word(p2), word(match_id)))
        return true
    end
    nk.logger_info(string.format("ikemen %s: pairing seq=%s %s-%s route=%s rtt=%d upgrades=%d id=%s",
        where, word(pairing), word(p1), word(p2), route, math.floor(tonumber(rtt) or 0), upgrades or 0, word(match_id)))
    return true
end

return M
