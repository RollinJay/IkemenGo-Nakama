-- Nakama online play: from a matchmaker or lobby pairing into IKEMEN netplay.
--
-- Nakama only passes connection details. Once two players are paired, both
-- clients join the pairing's Nakama match, trade UDP candidates through it and
-- punch a path through their NATs (nakama.startP2P). The netplay session then
-- runs over that path (nakama.enterNetPlay). From connected() on, the flow is
-- the one Host Game / Join Game use: synchronize(), then the netplay menu.

local online = {}

-- How long each step may take, in frames (60 per second).
online.connectFrames = 60 * 15
online.p2pFrames = 60 * 25
online.sessionFrames = 60 * 15

-- Status and failure texts. A screenpack or module can replace them.
online.text = {
	connecting = 'Connecting to the online service...',
	searching = 'Searching for an opponent...',
	found = 'Opponent found. Opening a connection...',
	session = 'Starting the netplay session...',
	service = 'Could not reach the online service.',
	match = 'Matchmaking stopped before an opponent was found.',
	p2p = 'Could not open a connection to the other player.\nOne of the two networks may block direct connections.',
	session_failed = 'The netplay session with the other player could not start.',
}

-- True while a Nakama netplay session owns the P2P socket and the match.
-- start.f_leaveOnlineSession leaves both alone until online.f_leave runs.
local sessionActive = false

function online.f_sessionActive()
	return sessionActive
end

local function status()
	if nakama == nil or nakama.status == nil then
		return {connected = false, matchmaking = false, match_id = '', p2p = 'none', netplay = 'none'}
	end
	return nakama.status()
end

-- nakama.status(), or a disconnected status when Nakama is unavailable.
function online.f_status()
	return status()
end

-- Draws the "connecting" overlay used by Host Game / Join Game when a
-- screenpack is loaded; the command-line path only keeps the window alive.
local function drawStatus(text)
	if motif ~= nil and motif.title_info ~= nil and main.background ~= nil and motif[main.background] ~= nil then
		local bg = motif[main.background]
		clearColor(bg.bgclearcolor[1], bg.bgclearcolor[2], bg.bgclearcolor[3])
		bgDraw(bg.BGDef, 0)
		rectDraw(motif.title_info.connecting.overlay.RectData)
		textImgReset(motif.title_info.connecting.TextSpriteData)
		textImgSetText(motif.title_info.connecting.TextSpriteData, text)
		textImgDraw(motif.title_info.connecting.TextSpriteData)
		bgDraw(bg.BGDef, 1)
	end
	refresh()
end

local function cancelled(interactive)
	if esc() then
		return true
	end
	if interactive and motif ~= nil and getInput(-1, motif.title_info.menu.cancel.key) then
		sndPlay(motif.Snd, motif.title_info.cancel.snd[1], motif.title_info.cancel.snd[2])
		return true
	end
	return false
end

-- Calls step() once per frame until it returns true or false. Returns that
-- value, 'cancel' or 'timeout'. frames == nil waits without a limit. key
-- names the online.text entry shown meanwhile; draw(key), when given, draws
-- the frame instead (the lobby draws its own screens) and must end with
-- refresh(). draw may instead return a reason key to stop waiting; wait
-- then returns that key.
local function wait(key, frames, interactive, step, draw)
	local n = 0
	while frames == nil or n < frames do
		local r = step()
		if r ~= nil then
			return r
		end
		if cancelled(interactive) then
			return 'cancel'
		end
		if draw ~= nil then
			local stop = draw(key)
			if stop ~= nil then
				return stop
			end
		else
			drawStatus(online.text[key])
		end
		n = n + 1
	end
	return 'timeout'
end

-- Connects to the Nakama server from the client profile when needed.
-- draw(key), when given, draws the frame while connecting.
function online.f_ensureConnected(interactive, draw)
	if nakama == nil then
		return false
	end
	if status().connected then
		return true
	end
	nakama.connect()
	return wait('connecting', online.connectFrames, interactive, function()
		local st = status()
		if st.connected then
			return true
		end
		if st.error ~= nil then
			return false
		end
	end, draw) == true
end

-- Ends a netplay session opened with f_openSession but stays in the Nakama
-- match: a lobby member keeps their place in the lobby between matches.
-- Call exitNetPlay() first.
function online.f_endSession()
	sessionActive = false
	if nakama ~= nil and nakama.stopP2P ~= nil then
		nakama.stopP2P()
	end
end

-- Leaves the Nakama match and closes the P2P socket. Safe to call any time.
function online.f_leave()
	sessionActive = false
	if nakama == nil then
		return
	end
	pcall(nakama.cancelMatchmaking)
	if nakama.stopP2P ~= nil then
		nakama.stopP2P()
	end
	if nakama.currentMatch ~= nil and nakama.currentMatch() ~= '' then
		pcall(nakama.leaveMatch)
	end
end

-- Queues for a 1v1 match ('unranked' or 'ranked') and waits until the
-- matchmaker has paired this client and it has joined the pairing's match.
function online.f_findMatch(mode, interactive)
	online.f_leave()
	nakama.clearError()
	local ok = pcall(nakama.matchmake, mode or 'unranked')
	if not ok then
		return false
	end
	local r = wait('searching', nil, interactive, function()
		local st = status()
		if st.match_id ~= '' and st.host ~= nil then
			return true
		end
		if st.error ~= nil or not st.connected then
			return false
		end
	end)
	if r ~= true then
		pcall(nakama.cancelMatchmaking)
	end
	return r == true, r
end

-- The failure reason of a wait() result: 'cancel' or a reason key returned
-- by draw is kept; failures and timeouts become fallback.
local function reasonOf(r, fallback)
	if type(r) == 'string' and r ~= 'timeout' then
		return r
	end
	return fallback
end

-- Opens the P2P path to the paired player and starts the netplay session on
-- it. host overrides the pairing's role (lobby UIs pass it explicitly);
-- draw(key) replaces the status screen ('found', then 'session') and may
-- return a reason key to give up (the lobby does when the pairing has ended).
-- Returns true once connected(); on failure returns false and a reason key.
function online.f_openSession(interactive, host, draw)
	local stun = getCommandLineValue('-nakama-stun') or ''
	local ok = pcall(nakama.startP2P, gameOption('Netplay.Rollback.Port'), stun)
	if not ok then
		return false, 'p2p'
	end
	local r = wait('found', online.p2pFrames, interactive, function()
		local st = status()
		if st.p2p == 'ready' then
			return true
		end
		if st.p2p == 'failed' or st.p2p == 'closed' or st.match_id == '' then
			return false
		end
	end, draw)
	if r ~= true then
		return false, reasonOf(r, 'p2p')
	end
	if host == nil then
		ok = pcall(nakama.enterNetPlay)
	else
		ok = pcall(nakama.enterNetPlay, host)
	end
	if not ok then
		return false, 'session_failed'
	end
	sessionActive = true
	r = wait('session', online.sessionFrames, interactive, function()
		if connected() then
			return true
		end
		if status().netplay == 'failed' then
			return false
		end
	end, draw)
	if r ~= true then
		exitNetPlay()
		sessionActive = false
		return false, reasonOf(r, 'session_failed')
	end
	-- The session's replays: one per match, and the whole session's for the
	-- matches those do not cover (replay.lua).
	replay.f_record()
	return true
end

-- Matchmaking through to a connected netplay session. Returns true, or false
-- and a reason key: 'service', 'match', 'p2p', 'session_failed' or 'cancel'.
function online.f_connect(mode, interactive)
	online.matchMode = mode
	if not online.f_ensureConnected(interactive) then
		return false, 'service'
	end
	local found, r = online.f_findMatch(mode, interactive)
	if not found then
		return false, r == 'cancel' and 'cancel' or 'match'
	end
	local ok, reason = online.f_openSession(interactive)
	if not ok then
		online.f_leave()
	end
	return ok, reason
end

function online.f_reasonText(reason)
	local text = online.text[reason] or tostring(reason)
	local st = status()
	if reason == 'service' and st.error ~= nil then
		text = text .. '\n' .. st.error
	elseif reason == 'session_failed' and st.netplay_error ~= nil then
		text = text .. '\n' .. st.netplay_error
	end
	return text
end

-- The fight screen shows each player's online name in place of the name of
-- the character they play as (nakama.setMatchNames). The names are read
-- from the server; the matchmaker pairing's host starts on side 1, and a
-- ranked set that switches sides moves the names with the players.
--
-- The names are per side, so they show in a fight between the two players,
-- one on each side. A co-op fight has both players on side 1 and the CPU on
-- side 2; its fight screen keeps the characters' names (online.f_fightNames).
--
-- The reply can arrive after the session has ended: namesRequest tells the
-- current request from earlier ones, and online.f_clearMatchNames ends it,
-- so that a late reply cannot set names for a later match.
local namesRequest = 0

-- The session's names and user IDs for sides 1 and 2, once they arrived.
local matchNames = nil

-- Whether the fight about to start is between the two players, one on each
-- side (not a co-op mode or a fight against the CPU).
local function versusFight()
	return not main.coop and not main.cpuSide[1] and not main.cpuSide[2]
end

-- Just before each fight of a matchmade session (hook start.f_game): the
-- players' names for a fight between them, the characters' names otherwise.
function online.f_fightNames()
	if matchNames == nil or nakama.setMatchNames == nil then
		return
	end
	if versusFight() then
		pcall(nakama.setMatchNames, matchNames[1], matchNames[2], matchNames[3], matchNames[4])
	else
		pcall(nakama.setMatchNames)
	end
end

function online.f_setMatchNames()
	if nakama.getUsers == nil or nakama.setMatchNames == nil then
		return
	end
	local st = status()
	local me = nakama.userId()
	local peer = st.peer or ''
	if peer == '' or me == '' then
		return
	end
	local p1, p2 = peer, me
	if st.host then
		p1, p2 = me, peer
	end
	namesRequest = namesRequest + 1
	local request = namesRequest
	-- The match replays of the session name the accounts (replay.lua).
	local ctx = {kind = 'match', ranked = online.matchMode == 'ranked', accounts = {}, users = {p1, p2}}
	replay.context = ctx
	nakama.on('users', function(_, ev)
		if request ~= namesRequest or ev.error ~= nil or type(ev.payload) ~= 'table' then
			return
		end
		local names = {}
		for _, u in ipairs(ev.payload.users or {}) do
			names[u.user_id] = u.display_name or ''
			ctx.accounts[u.user_id] = {username = u.username or '', display_name = u.display_name or ''}
		end
		matchNames = {names[p1] or '', names[p2] or '', p1, p2}
		pcall(nakama.setMatchNames, matchNames[1], matchNames[2], p1, p2)
	end, 'online')
	pcall(nakama.getUsers, {p1, p2})
end

-- A member's tag: four digits from the account ID (the lobby module's
-- tag_of), which tell players with the same name apart.
function online.f_tag(userId)
	local n = tonumber((tostring(userId or ''):gsub('-', ''):sub(1, 8)), 16)
	if n == nil then
		return ''
	end
	return string.format('%04d', n % 10000)
end

-- Clears the fight screen's online names, and ends a request for them that
-- has not been answered yet.
function online.f_clearMatchNames()
	namesRequest = namesRequest + 1
	matchNames = nil
	if nakama.setMatchNames ~= nil then
		pcall(nakama.setMatchNames)
	end
end

-- Ends what a matchmade session told its match replays.
function online.f_clearMatchContext()
	online.f_clearMatchNames()
	if replay.context ~= nil and replay.context.kind == 'match' then
		replay.context = nil
	end
end

-- Title menu item: matchmaking, then the same netplay menu Host Game and
-- Join Game lead to. enterMenu and showWarning are main.lua's local helpers.
function online.f_menuMatch(mode, t, item, enterMenu, showWarning)
	main.f_waitForPreloads(true)
	local doneSnd = motif[main.group].cursor.done.snd[t[item].itemname] or motif[main.group].cursor.done.snd.default
	sndPlay(motif.Snd, doneSnd[1], doneSnd[2])
	hook.run("main.t_itemname", t, item)
	local ok, reason = online.f_connect(mode, true)
	if ok then
		online.f_setMatchNames()
		if synchronize() then
			enterMenu()
		end
		replayStop()
		exitNetPlay()
		exitReplay()
		online.f_clearMatchContext()
		showWarning()
	elseif reason ~= 'cancel' then
		main.f_warning(online.f_reasonText(reason), motif[main.group], motif[main.background])
	end
	online.f_leave()
	esc(false)
	return nil
end

hook.add('start.f_game', 'online', online.f_fightNames)

return online
