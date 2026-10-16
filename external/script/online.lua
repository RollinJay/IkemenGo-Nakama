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

-- Status and failure texts. A screenpack or module can replace them. In
-- the texts of ranked levels, {name} stands for a value the server sends
-- (nakama/README.md, Levels): the server writes the numbers, with thousands
-- separators. The engine's default font for these screens (f-6x9) draws
-- ASCII only, hence "->" for an arrow.
online.text = {
	connecting = 'Connecting to the online service...',
	searching = 'Searching for an opponent...',
	-- Under searching, in a ranked search: the player's level.
	searching_level = '{label} {level}',
	found = 'Opponent found. Opening a connection...',
	session = 'Starting the netplay session...',
	service = 'Could not reach the online service.',
	match = 'Matchmaking stopped before an opponent was found.',
	p2p = 'Could not open a connection to the other player.\nOne of the two networks may block direct connections.',
	session_failed = 'The netplay session with the other player could not start.',
	-- The screen after a ranked set: who won it and the player's level
	-- before and after it, or that it is disputed.
	ranked_won = 'You won the ranked set.',
	ranked_lost = 'You lost the ranked set.',
	ranked_level = '{label} {before} -> {after} ({change})',
	ranked_disputed = 'Result disputed: a moderator will decide.',
	ranked_waiting = 'The ranked set is over.\nWaiting for its result...',
}

-- How long the screen after a ranked set shows, in frames. In a netplay
-- session both games show it for exactly this long: their menus run in
-- lockstep, so its length cannot depend on when the result arrives.
online.rankedResultFrames = 60 * 4

-- text with each {name} replaced by values[name] ({name} stays when values
-- has no such value).
function online.f_fill(text, values)
	return (string.gsub(text or '', '{([%w_]+)}', function(name)
		local v = values[name]
		if v == nil then
			return nil
		end
		return tostring(v)
	end))
end

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

-- The player's rating for a ranked search (the answer of ikemen_get_rating:
-- rating, level, level_text, label), asked for when the player queues.
local searchMode = nil
local searchRating = nil
local ratingRequest = 0

-- The text of a status key, with the player's level under "searching" in a
-- ranked search once the server has sent it.
function online.f_statusText(key)
	local text = online.text[key] or tostring(key)
	if key == 'searching' and searchMode == 'ranked' and type(searchRating) == 'table' and searchRating.level_text ~= nil then
		text = text .. '\n' .. online.f_fill(online.text.searching_level, {label = searchRating.label or '', level = searchRating.level_text})
	end
	return text
end

-- Asks the server for the player's rating and level, for the ranked search
-- screen (the "rating" event brings it).
local function requestRating()
	searchRating = nil
	if nakama == nil or nakama.getRating == nil then
		return
	end
	ratingRequest = ratingRequest + 1
	local request = ratingRequest
	nakama.on('rating', function(_, ev)
		if request ~= ratingRequest then
			return
		end
		if ev.error ~= nil or type(ev.payload) ~= 'table' then
			print('Ranked rating: ' .. tostring(ev.error))
			return
		end
		searchRating = ev.payload
		if searchRating.level_text ~= nil then
			print('Ranked search: ' .. online.f_fill(online.text.searching_level, {label = searchRating.label or '', level = searchRating.level_text}))
		end
	end, 'online')
	pcall(nakama.getRating)
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
			drawStatus(online.f_statusText(key))
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
	searchMode = mode or 'unranked'
	if searchMode == 'ranked' then
		requestRating()
	end
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

-- Ranked sets. A matchmade ranked session plays best-of-N sets; the ranked
-- set's state is kept by the engine (nakama.rankedSet), and start.f_game
-- records each match's winner in it. When a set has been decided (by its
-- last match or a forfeit), each player's game claims the set's winner with
-- the server, which changes the two players' ratings once both claims agree
-- (nakama/README.md, Ranked sets). The claim goes once per set, from both
-- players' games; it does not wait for the answer, which arrives as the
-- "ranked_result" event.
function online.f_submitRankedSet()
	if nakama == nil or nakama.submitRankedSet == nil then
		return false
	end
	local ok, sent = pcall(nakama.submitRankedSet)
	if not ok then
		print('Ranked set claim failed: ' .. tostring(sent))
		return false
	end
	return sent == true
end

-- Waits up to frames frames (60 per second) for the server's answer to the
-- claim of a ranked set; for the command line, which quits right after its
-- match. Returns the answer's payload, or nil.
function online.f_waitRankedResult(frames)
	local answer = nil
	nakama.on('ranked_result', function(_, ev)
		answer = ev
	end, 'online_wait')
	for _ = 1, frames do
		if answer ~= nil then
			break
		end
		refresh()
	end
	nakama.on('ranked_result', nil, 'online_wait')
	if answer == nil then
		return nil
	end
	if answer.error ~= nil then
		print('Ranked set claim: ' .. tostring(answer.error))
		return nil
	end
	return answer.payload
end

if nakama ~= nil and nakama.on ~= nil then
	nakama.on('ranked_result', function(_, ev)
		if ev.error ~= nil then
			print('Ranked set claim: ' .. tostring(ev.error))
		elseif type(ev.payload) == 'table' then
			print('Ranked set claim: ' .. tostring(ev.payload.status))
		end
	end, 'online')
end

-- The results of ranked sets the server sent (its notifications
-- ikemen_ranked_settled and ikemen_ranked_disputed, nakama/README.md,
-- Levels), by set ID: {subject, content}.
local rankedResults = {}

-- The ID of the ranked set being played, or just decided: the session's
-- match ID for its first set, "<match ID>#<n>" for its n-th.
function online.f_rankedSetId()
	if nakama == nil or nakama.rankedSet == nil then
		return ''
	end
	local rs = nakama.rankedSet()
	local id = tostring(rs.match_id or '')
	if id == '' and nakama.currentMatch ~= nil then
		id = nakama.currentMatch()
	end
	local n = tonumber(rs.set) or 1
	if id ~= '' and n > 1 and not string.find(id, '#', 1, true) then
		id = id .. '#' .. n
	end
	return id
end

-- The text of a ranked set's result, or nil while it has not come.
function online.f_rankedResultText(setId)
	local r = rankedResults[setId or '']
	if r == nil then
		return nil
	end
	if r.subject == 'ikemen_ranked_disputed' then
		return online.text.ranked_disputed
	end
	local c = r.content
	local head = c.won == true and online.text.ranked_won or online.text.ranked_lost
	return head .. '\n' .. online.f_fill(online.text.ranked_level, {
		label = c.label or '',
		before = c.level_before_text or '',
		after = c.level_text or '',
		change = c.change_text or '',
	})
end

if nakama ~= nil and nakama.on ~= nil then
	nakama.on('notification', function(_, ev)
		local p = ev.payload
		if type(p) ~= 'table' or type(p.content) ~= 'table' then
			return
		end
		if p.subject ~= 'ikemen_ranked_settled' and p.subject ~= 'ikemen_ranked_disputed' then
			return
		end
		local id = tostring(p.content.match_id or '')
		if id == '' then
			return
		end
		rankedResults[id] = {subject = p.subject, content = p.content}
		print('Ranked set ' .. id .. ': ' .. string.gsub(online.f_rankedResultText(id), '\n', ' '))
	end, 'online')
end

-- The screen after a ranked set: its result once the server has sent it,
-- shown for frames frames (on the "connecting" overlay, with a
-- screenpack). With waitFrames, it first waits up to that many frames for
-- the result: only outside a netplay session's menus (the command line),
-- whose games must run the same frames. Returns true when the result came.
function online.f_showRankedResult(setId, frames, waitFrames)
	local function text()
		return online.f_rankedResultText(setId) or online.text.ranked_waiting
	end
	for _ = 1, waitFrames or 0 do
		if rankedResults[setId] ~= nil then
			break
		end
		drawStatus(text())
	end
	for _ = 1, frames do
		drawStatus(text())
	end
	return rankedResults[setId] ~= nil
end

-- After the last match of a ranked set in a netplay session (start.lua):
-- the screen after the set, for online.rankedResultFrames.
function online.f_rankedSetOver()
	local id = online.f_rankedSetId()
	if id ~= '' then
		online.f_showRankedResult(id, online.rankedResultFrames)
	end
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
