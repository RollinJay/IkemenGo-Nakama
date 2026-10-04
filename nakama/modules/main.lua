-- Nakama runtime entry point for the IKEMEN GO integration.
--
-- Nakama loads every .lua file in the runtime path. Lua authoritative match
-- handlers are not registered by name: nk.match_create("ikemen_session", ...)
-- resolves the handler with require("ikemen_session"), so the session/lobby
-- modules only need to return their callback tables.

local nk = require("nakama")

-- These modules register their own RPC/matchmaker handlers when required.
require("ikemen_matchmaker")
require("ikemen_rating")
require("ikemen_lobby")
require("ikemen_account")

nk.logger_info("IKEMEN GO Nakama modules loaded")
