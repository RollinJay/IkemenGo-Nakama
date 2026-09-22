-- Nakama runtime entry point for the IKEMEN GO integration.
--
-- Nakama loads modules/main.lua as the Lua runtime entry point. Keep the
-- feature modules separate so the game-specific deployment can replace or
-- extend individual handlers without changing the engine client API.

local nk = require("nakama")
local session = require("ikemen_session")
local lobby = require("ikemen_lobby")

nk.register_match("ikemen_session", session)
nk.register_match("ikemen_lobby", lobby)

-- These modules register their own RPC/matchmaker handlers when required.
require("ikemen_matchmaker")
require("ikemen_rating")

nk.logger_info("IKEMEN GO Nakama modules loaded")
