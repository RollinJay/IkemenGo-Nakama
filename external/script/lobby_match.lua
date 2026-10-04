-- One online lobby match (external/script/lobby.lua sets main.luaPath to
-- this file): a single fight, then back to the lobby room. The winning side
-- (1 or 2, 0 for a draw) is left in lobby.matchWinner; it stays nil when the
-- fight did not take place. Just before the fight, the selection goes to
-- the members who watch it (lobby.f_publishSelection).

local winner = nil
local game = start.f_game
start.f_game = function(common)
	lobby.f_publishSelection()
	local result = game(common)
	winner = result
	return result
end
local ok, err = pcall(launchFight, {})
start.f_game = game
if not ok then
	error(err, 0)
end
lobby.matchWinner = winner
setMatchNo(-1)
start.exit = true
