-- Match replays (src/replay_match.go, src/replay_local.go). A match is saved
-- to replay.dir as a file that describes it beside its inputs: the characters
-- (by their select.def entries and definition files), palettes, team modes,
-- stage, rounds and game mode, and, online, the players' accounts (user ID,
-- username, online name and tag). The replay menu plays such a file by
-- loading the fight it describes, as a lobby spectator loads a watched
-- match, so a roster or screenpack changed since the recording does not
-- change what it loads.
--
-- Which matches are saved:
--   - online, with rollback netcode: every match of the session, versus or
--     co-op. The replay of the whole session, recorded as before, is removed
--     when every match of the session has a match replay;
--   - local versus (players on both sides: VS Mode, Team Versus, Versus
--     Co-op, and a challenger's match in arcade), when the Local Versus
--     Replays option (Config.LocalVersusReplays) is on;
--   - matches against the CPU in the modes a game lists in
--     replay.recordModes (none by default), such as the modes of its score
--     leaderboards. Training is never saved: its settings change the match
--     outside its inputs.
-- replayLastMatchFile() returns the file of the last match saved.

local replay = {}

replay.dir = 'save/replays'

-- Game modes whose matches against the CPU are saved, by mode name:
-- replay.recordModes.arcade = true saves arcade matches.
replay.recordModes = {}

-- Local versus modes: saved when Config.LocalVersusReplays is on.
replay.versusModes = {versus = true, versuscoop = true, challenger = true}

-- What the flow that opened the session knows about its matches beyond the
-- fight: kind ('lobby' or 'match'), ranked, the lobby's name, room ID and
-- pairing, the players' accounts by user ID ({username, display_name}) and,
-- after matchmaking, the user IDs of the session's host and the other
-- player (users). lobby.lua and online.lua set it; a direct netplay session
-- has none.
replay.context = nil

-- True while a match replay plays (replay.f_play).
replay.playing = false

-- The most names listed in a message about a replay's content.
replay.listMax = 6

-- Starts saving the session: the replay of the whole session and, with
-- rollback netcode, one match replay per match.
function replay.f_record()
	if gameOption('Netplay.RollbackNetcode') then
		-- A folder that cannot be written leaves the session unrecorded, as a
		-- session replay that cannot be created does.
		local ok, err = pcall(replaySaveMatches, replay.dir)
		if not ok then
			print('Match replays are off: ' .. tostring(err))
		end
	end
	replayRecord(replay.dir .. '/' .. os.date("%Y-%m-%d_%Hh%Mm%Ss") .. '.replay')
end

-- The name of a team mode number (0 to 3) in a description.
function replay.f_teamModeName(mode)
	return ({[0] = 'single', [1] = 'simul', [2] = 'turns', [3] = 'tag'})[mode] or 'single'
end
local teamModeName = replay.f_teamModeName

-- The fight about to start: team modes, and each side's characters in fight
-- order (after order select) with their roster refs, select.def entries,
-- definition files, names and palettes.
function replay.f_selection()
	local out = {
		p1teammode = teamModeName(start.p[1].teamMode),
		p2teammode = teamModeName(start.p[2].teamMode),
		p1 = {},
		p2 = {},
	}
	for side = 1, 2 do
		local remap = start.t_orderRemap and start.t_orderRemap[side] or {}
		local selected = start.p[side].t_selected or {}
		for slot = 1, #selected do
			local v = selected[remap[slot] or slot]
			if v ~= nil then
				local c = start.f_getCharData(v.ref) or {}
				table.insert(out['p' .. side], {ref = v.ref, pal = v.pal, char = c.char, def = c.def, name = c.name})
			end
		end
	end
	return out
end

-- Whether the fight about to start is between two players, one on each side
-- (with their CPU partners): not a co-op mode or a fight against the CPU.
local function versusFight()
	return not main.coop and not main.cpuSide[1] and not main.cpuSide[2]
end

-- The players of the online match about to start, with their accounts: in a
-- versus fight, the players on sides 1 and 2 as the fight screen names them
-- (nakama.setMatchNames; before the names have arrived from the server, the
-- session's users); in a co-op fight, the session's host and the other
-- player, members 1 and 2 of side 1.
local function players(ctx, versus, coop)
	local out = {}
	if nakama == nil or nakama.matchUsers == nil then
		return out
	end
	local u1, u2, names = '', '', {'', ''}
	if versus then
		local ok, a, b = pcall(nakama.matchUsers)
		if ok then
			u1, u2 = a or '', b or ''
		end
		local okNames, n1, n2 = pcall(nakama.matchNames)
		if okNames then
			names = {n1 or '', n2 or ''}
		end
	end
	if u1 == '' and u2 == '' and type(ctx.users) == 'table' then
		u1, u2 = ctx.users[1] or '', ctx.users[2] or ''
	end
	local accounts = ctx.accounts or {}
	for i, uid in ipairs({u1, u2}) do
		if uid ~= '' then
			local account = accounts[uid] or {}
			local name = names[i]
			if name == '' then
				name = account.display_name or ''
			end
			local p = {
				side = i,
				user_id = uid,
				username = account.username or '',
				display_name = name,
				tag = online.f_tag(uid),
			}
			if coop then
				p.side, p.member = 1, i
			end
			table.insert(out, p)
		end
	end
	return out
end

-- The description of the match about to start. sel is its selection
-- (replay.f_selection's shape); rules has the game mode (mode), the rounds
-- to win per team mode (matchwins, main.matchWins' shape), the round time in
-- seconds (roundtime), the select.def parameters in use (charparam), and
-- whether the fight is between two players, one on each side (versus,
-- default true) or two players against the CPU on side 1 (coop, default
-- false). kind is 'local' for an offline match; an online one takes the
-- session's (replay.context). The engine also records the rules, game
-- parameters, AI levels and match context it starts the match with, and the
-- players' state, and applies those when the replay plays.
function replay.f_info(sel, rules, kind)
	local versus, coop = rules.versus ~= false, rules.coop == true
	local info = {
		version = 1,
		kind = kind,
		mode = rules.mode,
		selection = sel,
		-- Whether the fight screen showed the players' online names.
		names = false,
		coop = coop,
		matchwins = rules.matchwins,
		roundtime = rules.roundtime,
		charparam = rules.charparam,
	}
	if kind ~= 'local' then
		local ctx = replay.context or {}
		info.kind = ctx.kind or 'netplay'
		info.ranked = ctx.ranked
		info.lobby = ctx.lobby
		info.players = players(ctx, versus, coop)
		info.names = versus
	end
	return info
end

-- Describes the netplay match about to start for its match replay (sel and
-- rules as for replay.f_info).
function replay.f_describe(sel, rules)
	if not connected() then
		return
	end
	pcall(replaySetMatchInfo, replay.f_info(sel, rules))
end

-- The rules of the fight about to start, for replay.f_info.
local function fightRules()
	return {
		mode = gameMode(),
		matchwins = main.f_tableCopy(main.matchWins),
		roundtime = main.roundTime,
		charparam = {time = main.charparam.time, rounds = main.charparam.rounds},
		versus = versusFight(),
		coop = main.coop == true,
	}
end

-- Whether the offline fight about to start is saved: a local versus fight
-- (players on both sides, one or more each) when the option is on, a fight
-- against the CPU in a mode listed in replay.recordModes.
function replay.f_recordsLocal()
	local mode = gameMode()
	if mode == 'training' or replay.playing or main.replayActive then
		return false
	end
	if not main.cpuSide[1] and not main.cpuSide[2] then
		return replay.versusModes[mode] == true and gameOption('Config.LocalVersusReplays') == true
	end
	return replay.recordModes[mode] == true
end

-- Just before a fight (hook start.f_game): describes an online fight for its
-- match replay, and starts saving an offline fight that is saved.
function replay.f_beforeFight()
	if connected() then
		replay.f_describe(replay.f_selection(), fightRules())
		return
	end
	if not replay.f_recordsLocal() then
		return
	end
	local ok, err = pcall(replayRecordLocal, replay.dir, replay.f_info(replay.f_selection(), fightRules(), 'local'))
	if not ok then
		print('Match replay: ' .. tostring(err))
	end
end

hook.add('start.f_game', 'replay', replay.f_beforeFight)

-- ---------------------------------------------------------------------------
-- Playing

local function text(key, fallback)
	local t = motif.warning_info.text.text or {}
	local v = t[key]
	if v == nil or v == '' then
		return fallback
	end
	return v
end

local function warn(message)
	main.f_warning(message, motif[main.group], motif.replaybgdef)
end

-- Whether path names a definition file inside the game's folder: a replay
-- can come from anyone, and names files the game loads.
local function gameDef(path)
	if type(path) ~= 'string' or path == '' or path:find('[,;=%c]') ~= nil then
		return false
	end
	local slashed = '/' .. (path:gsub('\\', '/')) .. '/'
	return not path:match('^[/\\]') and not path:match('^%a:') and not slashed:find('/%.%./')
		and path:lower():match('%.def$') ~= nil
end

-- Where this game has a replay's character: {ref, def} for its select.def
-- entry, or {def} for its definition file, which is added to the roster when
-- the replay plays. nil when this game has neither.
local function findChar(c)
	if type(c) ~= 'table' then
		return nil
	end
	if type(c.char) == 'string' and c.char ~= '' and main.t_charDef[c.char:lower()] ~= nil then
		local ref = main.t_charDef[c.char:lower()]
		return {ref = ref, def = (start.f_getCharData(ref) or {}).def or ''}
	end
	if gameDef(c.def) and main.f_fileExists(c.def) then
		return {def = c.def}
	end
	return nil
end

-- A name for a replay's character in a message.
local function charName(c)
	if type(c) ~= 'table' then
		return '?'
	end
	for _, v in ipairs({c.name, c.char, c.def}) do
		if type(v) == 'string' and v ~= '' then
			return v
		end
	end
	return '?'
end

-- A list of names, one per line, at most replay.listMax.
local function listText(items)
	local lines = {}
	for i, v in ipairs(items) do
		if i > replay.listMax then
			table.insert(lines, string.format(text('replaymore', '...and %d more'), #items - replay.listMax))
			break
		end
		table.insert(lines, v)
	end
	return table.concat(lines, '\n')
end

-- What a content difference names: the character and file, the stage, the
-- lifebar or a common file.
local function diffName(d, sel)
	local file = (d.file or ''):match('([^/]+)$') or d.file or ''
	local side, slot = (d.label or ''):match('^p(%d)%.(%d+) ')
	local what = file
	if side ~= nil then
		local c = sel['p' .. side] and sel['p' .. side][tonumber(slot)] or {}
		what = charName(c) .. ': ' .. file
	elseif d.label == 'stage' then
		what = text('replaystagefile', 'Stage') .. ': ' .. file
	elseif d.label == 'fight' then
		what = text('replayfightfile', 'Lifebar') .. ': ' .. file
	end
	return what .. ' (' .. text('replay' .. (d.status or 'changed'), d.status or 'changed') .. ')'
end

-- An error's message for players: without the script position a raised
-- error starts with.
local function errorText(err)
	return (tostring(err or ''):gsub('^[^%s]+%.lua:%d+: ', ''))
end

-- Plays the match replay at path: rp is replayMatchInfo's result, err its
-- error. Messages say what keeps it from playing; content that differs from
-- the recording is listed first, and the player chooses whether to play.
function replay.f_play(path, rp, err)
	if rp == nil then
		warn(string.format(text('replayfailed', 'The replay cannot be played:\n%s'), errorText(err)))
		return false
	end
	local info = type(rp.info) == 'table' and rp.info or {}
	local sel = info.selection
	if type(sel) ~= 'table' or type(sel.p1) ~= 'table' or type(sel.p2) ~= 'table' or #sel.p1 == 0 or #sel.p2 == 0 or (rp.stage or '') == '' then
		warn(string.format(text('replayfailed', 'The replay cannot be played:\n%s'), 'no match description'))
		return false
	end
	-- The characters and the stage, found in this game.
	local found = {p1 = {}, p2 = {}}
	local defs = {p1 = {}, p2 = {}}
	local missing = {}
	for _, key in ipairs({'p1', 'p2'}) do
		for _, c in ipairs(sel[key]) do
			local where = findChar(c)
			if where == nil then
				table.insert(missing, charName(c))
			else
				table.insert(found[key], where)
				table.insert(defs[key], where.def)
			end
		end
	end
	if #missing > 0 then
		warn(string.format(text('replaychars', 'This replay needs characters this game does not have:\n%s'), listText(missing)))
		return false
	end
	if not gameDef(rp.stage) or not main.f_fileExists(rp.stage) then
		warn(string.format(text('replaystage', 'This replay needs a stage this game does not have:\n%s'), tostring(rp.stage)))
		return false
	end
	-- Content that differs from the recording.
	local ok, diffs = pcall(replayMatchContent, path, defs, rp.stage)
	local changed = {}
	if not rp.same_engine then
		table.insert(changed, string.format(text('replayengine', 'Engine: %s (this game: %s)'), rp.engine or '?', rp.this_engine or '?'))
	end
	if ok and type(diffs) == 'table' then
		-- A character on several slots has its files listed once.
		local seen = {}
		for _, d in ipairs(diffs) do
			local name = diffName(d, sel)
			if not seen[name] then
				seen[name] = true
				table.insert(changed, name)
			end
		end
	end
	if #changed > 0 and not main.f_warning(string.format(text('replaycontent',
		'This game differs from the one that recorded the replay:\n%s\n\nThe replay may play differently.\nPress a key to play it, or Esc to go back.'), listText(changed)),
		motif[main.group], motif.replaybgdef) then
		return false
	end
	-- Characters known by their definition file join the roster now.
	local chars = {p1 = {}, p2 = {}}
	for _, key in ipairs({'p1', 'p2'}) do
		for i, where in ipairs(found[key]) do
			local ref = where.ref
			if ref == nil then
				local okRef, r = pcall(start.f_getCharRef, where.def)
				if not okRef or r == nil then
					warn(string.format(text('replayfailed', 'The replay cannot be played:\n%s'), where.def))
					return false
				end
				ref = r
			end
			local pal = tonumber(sel[key][i].pal)
			table.insert(chars[key], {ref = ref, pal = pal and math.floor(pal) or nil})
		end
	end
	local okPlay, errPlay = pcall(replayMatchPlay, path)
	if not okPlay then
		exitReplay()
		warn(string.format(text('replayfailed', 'The replay cannot be played:\n%s'), errorText(errPlay)))
		return false
	end
	-- The players' names, when their fight screens showed them (a versus
	-- fight; files that predate the flag hold versus fights only).
	local bySide = {}
	if info.names ~= false then
		for _, p in ipairs(info.players or {}) do
			if type(p) == 'table' and (p.side == 1 or p.side == 2) then
				bySide[p.side] = p
			end
		end
	end
	online.f_clearMatchNames()
	if bySide[1] ~= nil or bySide[2] ~= nil then
		local p1, p2 = bySide[1] or {}, bySide[2] or {}
		pcall(nakama.setMatchNames, p1.display_name or '', p2.display_name or '', p1.user_id or '', p2.user_id or '')
	end
	-- The match as it was set up: game mode, rounds and round time.
	main.f_default()
	if type(info.matchwins) == 'table' then
		for k, v in pairs(info.matchwins) do
			if type(v) == 'table' then
				main.matchWins[k] = {tonumber(v[1]) or 1, tonumber(v[2]) or 1}
			end
		end
	end
	if tonumber(info.roundtime) ~= nil then
		main.roundTime = tonumber(info.roundtime)
	end
	if type(info.charparam) == 'table' then
		main.charparam.time = info.charparam.time == true
		main.charparam.rounds = info.charparam.rounds == true
	end
	-- A side whose first player was the CPU is the CPU's.
	local levels = type(rp.ai_levels) == 'table' and rp.ai_levels or {}
	for side = 1, 2 do
		main.cpuSide[side] = (tonumber(levels[side]) or 0) > 0
	end
	main.pauseMenu = false
	setGameMode(type(info.mode) == 'string' and info.mode ~= '' and info.mode or 'netplayversus')
	setHomeTeam(1)
	start.f_selectReset(true)
	main.t_availableChars = main.f_tableCopy(start.f_getOrderChars())
	start.t_roster = {}
	-- A Turns side starts with the member the fight started with (a survival
	-- run skips the members already defeated).
	local numChars = {#chars.p1, #chars.p2}
	for side = 1, 2 do
		local offset = type(rp.turns_offset) == 'table' and math.floor(tonumber(rp.turns_offset[side]) or 0) or 0
		if sel['p' .. side .. 'teammode'] == 'turns' and offset > 0 and offset < numChars[side] then
			start.p[side].turnsOffset = offset
			numChars[side] = numChars[side] - offset
		end
	end
	-- Replay inputs are read per side: controller 1 is player 1 (the
	-- engine applies the recorded input slots when the fight starts). CPU
	-- players get the AI levels they had (start.f_difficulty). The match
	-- number is the recorded one once the fight starts; until then, no
	-- earlier match's results apply (start.f_matchPersistence).
	resetRemapInput()
	main.f_saveBaseRemapInput()
	start.replayAILevels = type(rp.ai_levels) == 'table' and rp.ai_levels or nil
	setMatchNo(1)
	replay.playing = true
	local okFight, errFight = pcall(launchFight, {
		p1char = chars.p1,
		p2char = chars.p2,
		p1teammode = sel.p1teammode,
		p2teammode = sel.p2teammode,
		p1numchars = numChars[1],
		p2numchars = numChars[2],
		stage = rp.stage,
		forceStage = true,
		vsscreen = false,
		p1orderselect = false,
		p2orderselect = false,
		continue = false,
		quickcontinue = true,
		victoryscreen = false,
		winscreen = false,
	})
	start.replayAILevels = nil
	replay.playing = false
	if not okFight then
		print('Replay: ' .. tostring(errFight))
	end
	online.f_clearMatchNames()
	exitReplay()
	setMatchNo(-1)
	main.f_default()
	esc(false)
	resetKey()
	return okFight
end

return replay
