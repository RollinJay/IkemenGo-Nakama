-- Nakama online play: from a matchmaker or lobby pairing into IKEMEN netplay.
--
-- Nakama only passes connection details. Once two players are paired, both
-- clients join the pairing's Nakama match, trade UDP candidates through it and
-- punch a path through their NATs (nakama.startP2P). The netplay session then
-- runs over that path (nakama.enterNetPlay), with rollback netcode whatever
-- the players' Netcode option says (that option is for Host Game / Join
-- Game). From connected() on, the flow is the one Host Game / Join Game
-- use: synchronize(), then the netplay menu.

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
	-- The screen after a ranked set: while it waits for the result, then
	-- who won it and the player's level before and after it, or another
	-- final answer about it.
	ranked_reporting = 'The ranked set is over.\nReporting the result...',
	ranked_waiting_other = 'The ranked set is over.\nWaiting for the other player\'s result...',
	ranked_pending = 'The result is still being reported;\nit will show when it arrives.',
	ranked_won = 'You won the ranked set.',
	ranked_lost = 'You lost the ranked set.',
	ranked_level = '{label} {before} -> {after} ({change})',
	ranked_settled = 'The ranked set is settled.',
	ranked_disputed = 'Result disputed: a moderator will decide.',
	ranked_void = 'The ranked set has no result (void).',
	ranked_refused = 'The server refused the result:\n{reason}',
	ranked_expired = 'The result could not be reported.',
	-- A result that came later, on the next screen ({result}: its text).
	ranked_last = 'Last ranked set: {result}',
}

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
	if key == 'searching' and searchMode == 'ranked' and online.f_lateResultText ~= nil and online.f_lateResultText() ~= nil then
		text = text .. '\n' .. online.f_lateResultText()
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
	-- matches those do not cover (replay.lua). A Nakama session always uses
	-- rollback netcode (nakama.enterNetPlay).
	replay.f_record(true)
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
-- players' games, and the engine sends it again until the server's answer
-- is final (the claims of the set's id: claims[id]).
local claims = {}

-- The final answers about ranked sets, by set ID: {kind, content, text}.
-- kind is settled, disputed, void, refused or expired; content the set's
-- result for this player (the ikemen_ranked_settled notification's content,
-- or the result of the server's answer to the claim), when settled.
local rankedResults = {}

-- Set IDs whose result the screen after the set has shown.
local shownResults = {}

-- The result of a set that came after the screen after the set had ended
-- without it (or for a claim of an earlier run of the game): {id, text}.
-- The next screen the game draws itself shows it as "Last ranked set: ...":
-- the ranked search screen, the menu with the online items, or the screen
-- after the next set (which clears it).
online.lateResult = nil

-- Set IDs whose screen after the set is to come or showing (from the
-- claim on): a result that comes meanwhile is not late.
local screenFor = {}

function online.f_submitRankedSet()
	if nakama == nil or nakama.submitRankedSet == nil then
		return false
	end
	local ok, sent, id = pcall(nakama.submitRankedSet)
	if not ok then
		print('Ranked set claim failed: ' .. tostring(sent))
		return false
	end
	if sent == true then
		id = id or online.f_rankedSetId()
		claims[id] = true
		screenFor[id] = true
	end
	return sent == true
end

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

-- The text of a settled set's result for this player: content has won,
-- label, level_before_text, level_text and change_text.
local function settledText(c)
	local head = c.won == true and online.text.ranked_won or online.text.ranked_lost
	if c.level_text == nil then
		return head
	end
	return head .. '\n' .. online.f_fill(online.text.ranked_level, {
		label = c.label or '',
		before = c.level_before_text or '',
		after = c.level_text or '',
		change = c.change_text or '',
	})
end

-- The text of a ranked set's result, or nil while it has not come.
function online.f_rankedResultText(setId)
	local r = rankedResults[setId or '']
	if r == nil then
		return nil
	end
	return r.text
end

-- Records the final answer about a set (from a notification or from the
-- answer to the claim); a settled result with content is kept over one
-- without. A result that comes when no screen after the set is showing it
-- becomes online.lateResult.
local function recordResult(id, kind, content, text)
	local had = rankedResults[id]
	if had ~= nil and (had.content ~= nil or content == nil) then
		return
	end
	rankedResults[id] = {kind = kind, content = content, text = text}
	print('Ranked set ' .. id .. ': ' .. string.gsub(text, '\n', ' '))
	if not screenFor[id] and not shownResults[id] and kind ~= 'expired' then
		online.lateResult = {id = id, text = text}
	end
end

if nakama ~= nil and nakama.on ~= nil then
	-- The answers to the claims (src/nakama_ranked_queue.go): payload
	-- {set_id, status, final, sends, ...the server's answer}.
	nakama.on('ranked_result', function(_, ev)
		local p = type(ev.payload) == 'table' and ev.payload or {}
		local id = tostring(p.set_id or p.match_id or '')
		if ev.error ~= nil then
			print('Ranked set ' .. id .. ' claim: ' .. tostring(p.status) .. ': ' .. tostring(ev.error))
		else
			print('Ranked set ' .. id .. ' claim: ' .. tostring(p.status))
		end
		if id == '' or p.final ~= true then
			return
		end
		claims[id] = nil
		if p.status == 'settled' then
			if type(p.result) == 'table' then
				recordResult(id, 'settled', p.result, settledText(p.result))
			else
				recordResult(id, 'settled', nil, online.text.ranked_settled)
			end
		elseif p.status == 'disputed' then
			recordResult(id, 'disputed', nil, online.text.ranked_disputed)
		elseif p.status == 'void' then
			recordResult(id, 'void', nil, online.text.ranked_void)
		elseif p.status == 'expired' then
			recordResult(id, 'expired', nil, online.text.ranked_expired)
		else
			recordResult(id, 'refused', nil, online.f_fill(online.text.ranked_refused, {reason = tostring(ev.error or '')}))
		end
	end, 'online')

	-- The server's notifications about ranked sets (ikemen_ranked_settled
	-- and ikemen_ranked_disputed, nakama/README.md, Levels) are final
	-- answers too.
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
		if p.subject == 'ikemen_ranked_disputed' then
			recordResult(id, 'disputed', nil, online.text.ranked_disputed)
		else
			recordResult(id, 'settled', p.content, settledText(p.content))
		end
	end, 'online')

	-- Claims an earlier run of the game could not deliver go now.
	if nakama.resumeRankedClaims ~= nil then
		pcall(nakama.resumeRankedClaims)
	end
end

-- online.lateResult as a line ("Last ranked set: ..."), or nil.
function online.f_lateResultText()
	if online.lateResult == nil then
		return nil
	end
	return online.f_fill(online.text.ranked_last, {result = online.lateResult.text})
end

-- How long the screen after a ranked set waits for the result at most: in
-- frames of the session (60 per second), which both games count the same,
-- and in seconds in all when this game waits alone (the other player has
-- left the session). Then how long it shows the result, in frames.
online.rankedWaitFrames = 60 * 10
online.rankedWaitSeconds = 10
online.rankedResultFrames = 60 * 4

-- The screen after a ranked set, in a netplay session's menus (start.lua)
-- and after a command-line ranked match (main.lua). The two games run these
-- menus in step, frame by frame on each other's inputs, so the screen ends
-- on what both games know: each game raises its ready bit
-- (nakama.setReady) once it has its final answer about the set (the
-- server's answer to its claim, or the server's notification; at once when
-- it sent no claim), and the screen waits until the first frame on which
-- both bits are up (nakama.ready), or rankedWaitFrames frames. Then each
-- game shows its own result, or that it is still being reported, for
-- rankedResultFrames frames. If the other player leaves the session
-- meanwhile, this game waits alone, until its own answer has come or
-- rankedWaitSeconds have passed since the screen began. A result that comes
-- later shows as online.lateResult. Returns true when the result came in
-- time.
function online.f_rankedSetOver()
	local id = online.f_rankedSetId()
	if id == '' or nakama == nil or nakama.setReady == nil then
		return false
	end
	screenFor[id] = true
	local function answered()
		return claims[id] == nil or rankedResults[id] ~= nil
	end
	local function lines(text)
		local last = online.f_lateResultText()
		if last ~= nil and online.lateResult.id ~= id then
			return text .. '\n' .. last
		end
		return text
	end
	pcall(nakama.setReady, false)
	local started = os.clock()
	local frames = 0
	local alone = false
	local how = 'timeout'
	while true do
		local _, running = nakama.sessionFrame()
		if not running and not alone then
			alone = true
			print('Ranked set ' .. id .. ': the other player left the session; waiting alone')
		end
		if alone then
			if answered() then
				how = 'answer'
				break
			end
			if os.clock() - started >= online.rankedWaitSeconds then
				break
			end
		else
			-- Only the bits of the frame both games run on now, and the
			-- frames counted, decide when the screen ends.
			if nakama.ready() then
				how = 'ready'
				break
			end
			if frames >= online.rankedWaitFrames then
				break
			end
			if answered() then
				pcall(nakama.setReady, true)
			end
		end
		drawStatus(lines(answered() and online.text.ranked_waiting_other or online.text.ranked_reporting))
		frames = frames + 1
	end
	local frame, running = nakama.sessionFrame()
	print(string.format('Ranked set %s: the wait ended (%s%s) after %d frames, at session frame %d',
		id, how, alone and ', alone' or '', frames, frame))
	pcall(nakama.setReady, false)
	for _ = 1, online.rankedResultFrames do
		local text = online.f_rankedResultText(id)
		if text ~= nil then
			shownResults[id] = true
		end
		drawStatus(lines(text or online.text.ranked_pending))
	end
	if online.lateResult ~= nil and online.lateResult.id ~= id then
		-- Shown on this screen.
		online.lateResult = nil
	end
	screenFor[id] = nil
	if rankedResults[id] == nil then
		print('Ranked set ' .. id .. ': no result yet; it shows when it arrives')
	end
	return rankedResults[id] ~= nil
end

-- The line of a late result on the menu with the online items.
local onlineItems = {onlinematch = true, onlineranked = true, onlinelobby = true}
hook.add('main.f_menuCommonDraw', 'online', function(t)
	local text = online.f_lateResultText()
	if text == nil or type(t) ~= 'table' or motif == nil or motif.title_info == nil then
		return
	end
	local found = false
	for _, item in ipairs(t) do
		if onlineItems[item.itemname] then
			found = true
			break
		end
	end
	if not found then
		return
	end
	local c = motif.title_info.connecting
	local lc = (motif.info and motif.info.localcoord) or {320, 240}
	textImgReset(c.TextSpriteData)
	textImgSetText(c.TextSpriteData, text)
	textImgSetPos(c.TextSpriteData, c.offset[1], math.floor(lc[2] * 0.86))
	textImgDraw(c.TextSpriteData)
	textImgReset(c.TextSpriteData)
end)

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
