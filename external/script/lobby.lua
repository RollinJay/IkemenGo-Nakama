-- Online lobbies: lobby search, lobby settings and the lobby room.
--
-- A lobby runs on the Nakama server (nakama/modules/ikemen_lobby.lua). The
-- server keeps the members, their ready states and the settings, and decides
-- who plays whom. This script shows the lobby and acts on it:
--   * lobby search lists public lobbies and joins one, or one by room ID,
--     and changes the player's online name (asked for on the first visit);
--   * lobby settings creates a lobby, or changes it for the host;
--   * the room shows the members. When this player's match starts, the
--     script plays it over a P2P netplay session (online.lua), reports the
--     result and returns to the room. The other members can watch the match
--     from the players' replay stream while it is played.
-- Every element comes from [Lobby Info] and [LobbyBgDef] in the screenpack.

local lobby = {}

-- Timings in frames (60 per second).
lobby.requestFrames = 60 * 12
lobby.pingInterval = 60 * 3
lobby.reportInterval = 60 * 15
lobby.inputGuardFrames = 12
-- Input ignored after a match: players mashing through the victory screen
-- would otherwise toggle READY in the room.
lobby.afterMatchGuardFrames = 60
-- Wait before listing lobbies after leaving a room: Nakama updates the
-- listing about once a second (match.label_update_interval_ms), and an
-- earlier list would still show this player in the room just left.
lobby.listDelayFrames = 75

-- Chat history kept in memory and the longest message the server accepts.
lobby.chatHistory = 50
lobby.chatLimit = 80

-- Online names: 2 to 16 characters, each one the lifebar's name fonts can
-- draw (the fight screen shows the name in place of the name of the
-- character the player controls). Names need not be unique: lobbies show a
-- tag from the account ID beside them. The server checks the length and
-- refuses control characters.
lobby.nameLength = {2, 16}
-- How long saving a name may take: a session refresh, a new sign-in and the
-- request itself can each take up to the request timeout (10 seconds).
lobby.accountFrames = 60 * 35
-- How long watching waits for the match's stream to start playing.
lobby.watchFrames = 60 * 30

-- Values offered by lobby settings, in order.
lobby.choices = {
	size = {2, 3, 4, 5, 6, 7, 8},
	format = {'queue', 'winner_stays_on', 'round_robin', 'bracket'},
	maxgames = {1, 2, 3, 4, 5, 7, 10, 15, 20, 99},
	rounds = {0, 1, 2, 3, 4, 5},
	time = {0, 30, 45, 60, 90, 99, -1},
	teams = {'any', 'single'},
	stage = {'select', 'random'},
	start = {'auto', 'host'},
	interval = {3, 5, 8, 10, 15, 20, 30},
	type = {false, true},
	-- Seconds a watched match runs behind the players; 0: nobody can watch.
	watch = {0, 1, 3, 5, 10},
}

-- Settings item -> field of the settings table sent to the server.
local FIELD = {
	size = 'size', format = 'format', maxgames = 'max_games', rounds = 'rounds',
	time = 'time', teams = 'teams', stage = 'stage', start = 'start',
	interval = 'interval', type = 'private', watch = 'watch',
}

lobby.defaultSettings = {
	name = '',
	comment = '',
	size = 8,
	format = 'queue',
	max_games = 3,
	rounds = 0,
	time = 0,
	teams = 'any',
	stage = 'select',
	start = 'auto',
	interval = 5,
	private = false,
	watch = 3,
}

-- Session state of the lobby this client is in.
local L = {
	state = nil,
	version = -1,
	stateFrame = 0,
	byId = {},
	me = nil,
	chat = {},
	muted = {},
	handledSeq = -1,
	kicked = false,
	pingTimer = 0,
	reportTimer = 0,
	reportedPing = nil,
	frame = 0,
	reportedConnection = nil,
	-- Replies to asynchronous requests, filled by the nakama callbacks below.
	listReply = nil,
	createReply = nil,
	findReply = nil,
	accountReply = nil,
	accountInfo = nil,
}
lobby.session = L

-- ---------------------------------------------------------------------------
-- Helpers

-- Screenpack texts write a line break as \n, which the engine decodes when it
-- draws a text. The lobby draws its texts as given (drawText), so that a "\n"
-- a player types in a name, lobby name or chat message stays two characters
-- instead of starting a line that could pass for another message. The
-- screenpack's own texts are decoded here instead, once per loaded motif.
local decodedInfo = nil

local function decodeTexts(t, seen)
	for k, v in pairs(t) do
		if type(v) == 'string' then
			if v:find('\\n') ~= nil then
				t[k] = (v:gsub('\\n', '\n'))
			end
		elseif type(v) == 'table' and not seen[v] then
			seen[v] = true
			decodeTexts(v, seen)
		end
	end
end

local function info()
	local t = motif.lobby_info
	if t ~= nil and t ~= decodedInfo then
		decodedInfo = t
		decodeTexts(t, {[t] = true})
	end
	return t
end

local function status()
	return online.f_status()
end

local function fmt(pattern, ...)
	if pattern == nil or pattern == '' then
		return ''
	end
	local ok, out = pcall(string.format, pattern, ...)
	if ok then
		return out
	end
	return pattern
end

local function copy(t)
	local out = {}
	for k, v in pairs(t or {}) do
		out[k] = v
	end
	return out
end

local function snd(pair)
	if pair ~= nil and pair[1] ~= nil and pair[1] >= 0 then
		sndPlay(motif.Snd, pair[1], pair[2])
	end
end

local function eventSnd(name)
	local event = info().event
	if event ~= nil and event.snd ~= nil then
		snd(event.snd[name])
	end
end

-- Removes the last UTF-8 character of text.
local function dropLastChar(text)
	local i = #text
	while i > 0 do
		local c = text:byte(i)
		if c < 0x80 or c >= 0xC0 then
			return text:sub(1, i - 1)
		end
		i = i - 1
	end
	return ''
end

local function charCount(text)
	local n = 0
	for i = 1, #text do
		local c = text:byte(i)
		if c < 0x80 or c >= 0xC0 then
			n = n + 1
		end
	end
	return n
end

-- Draws a text element at its offset plus (x, y). window clips it:
-- x1, y1, x2, y2 in lobby coordinates.
local function drawText(el, text, x, y, window)
	if el == nil or el.TextSpriteData == nil or text == nil or text == '' then
		return
	end
	local ts = el.TextSpriteData
	textImgReset(ts)
	if x ~= nil then
		textImgAddPos(ts, x, y)
	end
	if window ~= nil then
		textImgSetWindow(ts, window[1], window[2], window[3], window[4])
	end
	-- Literal: the screenpack's \n are already line breaks (info), and those
	-- typed by players stay as typed.
	textImgSetText(ts, text, true)
	textImgDraw(ts)
end

-- Width of text in lobby coordinates when drawn with el.
local function textWidth(el, text)
	if el == nil or el.TextSpriteData == nil or text == nil or text == '' then
		return 0
	end
	local w = textImgGetTextWidth(el.TextSpriteData, text)
	local scale = el.scale ~= nil and el.scale[1] or 1
	return w * scale
end

-- Draws a box at (x, y); gx, gy widen it to the right and upwards.
local function drawBox(box, x, y, gx, gy)
	if box == nil or not box.visible or box.RectData == nil then
		return
	end
	local c = box.coords
	gx, gy = gx or 0, gy or 0
	rectSetWindow(box.RectData, x + c[1], y + c[2] - gy, x + c[3] + gx + 1, y + c[4] + 1)
	rectUpdate(box.RectData)
	rectDraw(box.RectData)
end

local function hasArt(el)
	return el ~= nil and el.AnimData ~= nil and ((el.anim ~= nil and el.anim >= 0) or (el.spr ~= nil and el.spr[1] >= 0))
end

-- Draws an element (box, then sprite or animation, then text) at (x, y).
-- The box is placed relative to the element's offset. text replaces the
-- element's own text; %s-style patterns in the element's text are
-- formatted with the extra arguments.
local function drawElement(el, x, y, text, ...)
	if el == nil then
		return
	end
	local ox, oy = 0, 0
	if el.offset ~= nil then
		ox, oy = el.offset[1] or 0, el.offset[2] or 0
	end
	drawBox(el.box, x + ox, y + oy)
	if hasArt(el) then
		main.f_animPosDraw(el.AnimData, x, y)
	end
	local t = text
	if t == nil then
		t = fmt(el.text, ...)
	end
	drawText(el, t, x, y)
end

local function drawBackground()
	local bg = motif.lobbybgdef
	clearColor(bg.bgclearcolor[1], bg.bgclearcolor[2], bg.bgclearcolor[3])
	bgDraw(bg.BGDef, 0)
end

-- Draws a screen's panel.<name> boxes in name order.
local function drawPanels(panels)
	if panels == nil then
		return
	end
	local names = {}
	for name in pairs(panels) do
		table.insert(names, name)
	end
	table.sort(names)
	for _, name in ipairs(names) do
		drawBox(panels[name], 0, 0)
	end
end

local function drawForeground()
	bgDraw(motif.lobbybgdef.BGDef, 1)
end

-- Full-screen message (status.text.<key>) over the current screen.
local function drawStatus(key, ...)
	local s = info().status
	rectDraw(s.overlay.RectData)
	local text = s.text ~= nil and s.text[key] or nil
	drawText(s, fmt(text or key, ...))
end

local function playMusic(interrupt)
	local music = motif.Music
	if music ~= nil and music.lobby ~= nil and music.lobby[1] ~= nil and (music.lobby[1].bgm or '') ~= '' then
		playBgm({source = 'motif.lobby', interrupt = interrupt})
	elseif interrupt then
		playBgm({source = 'motif.title', interrupt = true})
	end
end

-- Menu items from a lobby menu's itemname order; include(name) filters them.
local function menuItems(menu, include, display)
	local t = {}
	for _, name in ipairs(menu.itemname_order or {}) do
		local text = menu.itemname[name]
		if text ~= nil and (include == nil or include(name)) then
			local shown = text
			if display ~= nil then
				shown = display(name, text) or text
			end
			table.insert(t, {itemname = name, displayname = shown, paramname = name})
		end
	end
	return t
end

-- A menu that keeps its cursor on the same item when its items change. When
-- that item is gone (SETTINGS while a match is played, for example), the
-- cursor moves to the next item that is still there (or the one before it),
-- never back to the first item: a player pressing confirm again should not
-- toggle READY by surprise.
local function newMenu(menu)
	return {sec = {menu = menu}, items = {}, item = 1, cursorPosY = 1, moveTxt = 0}
end

local function setMenuItems(m, items)
	local old = m.items or {}
	local index = {}
	for i, it in ipairs(items) do
		index[it.itemname] = i
	end
	local pick = nil
	for i = m.item, #old do
		pick = index[old[i].itemname]
		if pick ~= nil then
			break
		end
	end
	for i = m.item - 1, 1, -1 do
		if pick ~= nil then
			break
		end
		pick = index[old[i].itemname]
	end
	m.items = items
	m.item = pick or 1
	local visible = m.sec.menu.window.visibleitems
	if visible == nil or visible <= 0 then
		visible = #items
	end
	m.cursorPosY = math.min(m.item, math.max(1, visible))
	main.menuSnap = true
end

local function selectMenuItem(m, name)
	for i, it in ipairs(m.items) do
		if it.itemname == name then
			m.item = i
			local visible = m.sec.menu.window.visibleitems
			if visible == nil or visible <= 0 then
				visible = #m.items
			end
			m.cursorPosY = math.min(i, math.max(1, visible))
			main.menuSnap = true
			return
		end
	end
end

local function menuMove(m)
	if #m.items == 0 then
		return
	end
	m.cursorPosY, m.moveTxt, m.item = main.f_menuCommonCalc(m.items, m.item, m.cursorPosY, m.moveTxt, m.sec, info().cursor)
end

local function menuDraw(m, inactive)
	if #m.items == 0 then
		return
	end
	main.f_menuCommonDraw(m.items, m.item, m.cursorPosY, m.moveTxt, m.sec, motif.lobbybgdef, true,
		{skipBG0 = true, skipBG1 = true, skipTitle = true, forceInactive = inactive})
end

local function pressed(keys)
	return keys ~= nil and getInput(-1, keys)
end

local function cancelPressed(menu)
	if esc() then
		esc(false)
		return true
	end
	return pressed(menu.cancel.key)
end

-- Shows a message with the screenpack's warning screen. A button pressed
-- this frame (the confirm that led here) is consumed first: getInput reports
-- a press once per frame, so it does not also close the message.
local function warn(text)
	local menu = info().browser.menu
	pressed(menu.done.key)
	pressed(menu.cancel.key)
	main.f_warning(tostring(text or ''), {menu = menu}, motif.lobbybgdef)
	resetKey()
end

-- ---------------------------------------------------------------------------
-- Text entry

-- Edits entry.text from the keyboard for one frame. Returns true when
-- confirmed, false when cancelled, nil while typing.
--
-- The entry ends on a key event. When that key is also a player's button
-- (Return is Start in the default key configuration), the button's press
-- shows one frame later; the token guard keeps it from also working the
-- screen that follows, such as closing a message about the entry.
local function editText(entry, limit)
	local ti = info().textinput
	if esc() then
		esc(false)
		resetKey()
		resetTokenGuard()
		return false
	end
	if getKey(ti.confirm.keycode) then
		resetKey()
		resetTokenGuard()
		return true
	elseif getKey(ti.truncate.keycode) then
		entry.text = dropLastChar(entry.text)
	elseif getKey(ti.trim.keycode) then
		entry.text = ''
	elseif getKey(ti.paste.keycode) then
		entry.text = entry.text .. (getClipboardString() or '')
	else
		entry.text = entry.text .. getKeyText()
	end
	resetKey()
	while charCount(entry.text) > limit do
		entry.text = dropLastChar(entry.text)
	end
	return nil
end

-- Full-screen text entry (textinput.text.<key>). Returns the text, or nil
-- when cancelled. tick() runs every frame and may return true to abort.
function lobby.f_input(key, default, limit, tick)
	local ti = info().textinput
	local entry = {text = default or ''}
	resetKey()
	while true do
		if tick ~= nil and tick() then
			return nil
		end
		local done = editText(entry, limit)
		if done == true then
			return entry.text
		elseif done == false then
			snd(info().cancel.snd)
			return nil
		end
		drawBackground()
		rectDraw(ti.overlay.RectData)
		local prompt = ti.text ~= nil and ti.text[key] or ''
		drawText(ti, (prompt or '') .. '\n\n' .. entry.text .. '_')
		drawForeground()
		refresh()
	end
end

-- ---------------------------------------------------------------------------
-- Server replies and lobby state

-- Named handlers ('lobby'): another script's nakama.on(event, fn) for the
-- same events does not replace them.
if nakama ~= nil and nakama.on ~= nil then
	nakama.on('lobby_list', function(_, ev)
		L.listReply = ev
	end, 'lobby')
	nakama.on('lobby_created', function(_, ev)
		L.createReply = ev
	end, 'lobby')
	nakama.on('lobby_found', function(_, ev)
		L.findReply = ev
	end, 'lobby')
	nakama.on('account', function(_, ev)
		L.accountInfo = ev
	end, 'lobby')
	nakama.on('account_name', function(_, ev)
		L.accountReply = ev
	end, 'lobby')
end

local function addChat(entry)
	table.insert(L.chat, entry)
	while #L.chat > lobby.chatHistory do
		table.remove(L.chat, 1)
	end
end

-- The name lobbies show for a member: the online name, or the account's
-- username until the player chooses a name.
local function memberName(m)
	if m == nil then
		return ''
	end
	if (m.name or '') ~= '' then
		return m.name
	end
	return m.username or ''
end

local function nameOf(userId)
	if userId == nil then
		return ''
	end
	return memberName(L.byId[userId])
end

-- A member's tag: four digits from the account ID (online.f_tag, as the
-- lobby module's tag_of), which tell members with the same name apart.
local function tagOf(userId)
	return online.f_tag(userId)
end

-- The name the fight screen shows for a member: the online name the player
-- chose, or '' (the character's name) when there is none.
local function hudName(userId)
	local m = nil
	if userId ~= nil then
		m = L.byId[userId]
	end
	if m == nil then
		return ''
	end
	return m.display_name or ''
end

-- This player's online name, or the username until one is chosen.
local function myName()
	local name = ''
	if nakama.displayName ~= nil then
		name = nakama.displayName()
	end
	if name == '' then
		name = nakama.username()
	end
	return name
end

-- Pulls the latest lobby state and the queued lobby events.
function lobby.f_poll()
	L.frame = L.frame + 1
	if nakama == nil or nakama.lobbyState == nil then
		return
	end
	local s, version = nakama.lobbyState(L.version)
	if s ~= nil then
		L.state = s
		L.stateFrame = L.frame
		L.byId = {}
		for _, m in ipairs(s.members or {}) do
			L.byId[m.user_id] = m
		end
		L.me = L.byId[nakama.userId()]
	elseif version ~= L.version then
		L.state = nil
		L.byId = {}
		L.me = nil
	end
	L.version = version
	for _, ev in ipairs(nakama.lobbyEvents()) do
		if ev.kind == 'chat' then
			if not L.muted[ev.user_id] then
				addChat({kind = 'chat', name = ev.name or ev.username or '', tag = tagOf(ev.user_id), text = ev.text or ''})
				if ev.user_id ~= nakama.userId() then
					eventSnd('chat')
				end
			end
		elseif ev.kind == 'notice' then
			addChat({kind = 'notice', notice = ev.notice, name = ev.name or ev.username or '', tag = tagOf(ev.user_id)})
			if ev.notice == 'joined' then
				eventSnd('join')
			elseif ev.notice == 'left' or ev.notice == 'kicked' then
				eventSnd('leave')
			end
		elseif ev.kind == 'error' then
			addChat({kind = 'error', text = tostring(ev.error or '')})
			eventSnd('error')
		elseif ev.kind == 'kicked' then
			L.kicked = true
		end
	end
end

-- Seconds left in the results phase, counted down locally.
local function phaseRemaining()
	if L.state == nil then
		return 0
	end
	local left = (L.state.phase_remaining or 0) - (L.frame - L.stateFrame) / 60
	return math.max(0, math.ceil(left))
end

local function matchElapsed()
	local cur = L.state and L.state.current
	if cur == nil then
		return 0
	end
	return math.floor((cur.elapsed or 0) + (L.frame - L.stateFrame) / 60)
end

local function isHost()
	return L.me ~= nil and L.me.host == true
end

local function isQueueFormat(format)
	return format == 'queue' or format == 'winner_stays_on'
end

-- True when the current pairing includes this player and has not been played.
local function myPairingPending()
	local s = L.state
	if s == nil or s.phase ~= 'playing' or s.current == nil then
		return false
	end
	local me = nakama.userId()
	return s.current.seq ~= L.handledSeq and (s.current.player1 == me or s.current.player2 == me)
end

-- Number of bars a round trip shows with the ping element p (room.slot.ping
-- by default), or nil when it is unknown.
local function pingLevel(ping, p)
	p = p or info().room.slot.ping
	if ping == nil or ping < 0 then
		return nil
	end
	local level = 0
	for _, limit in ipairs(p.thresholds or {}) do
		if ping <= limit then
			level = level + 1
		end
	end
	return math.min(level, math.max(0, p.bars or level))
end

-- Measures the server round trip now and then and reports it to the lobby
-- when the number of bars it shows changes, together with the region and
-- the connection type (wired, wifi, mobile). The round trip to the player
-- this client plays is measured and reported by the engine while their
-- direct connection is open.
function lobby.f_tickPing()
	L.pingTimer = L.pingTimer - 1
	if L.pingTimer <= 0 then
		L.pingTimer = lobby.pingInterval
		pcall(nakama.ping)
	end
	L.reportTimer = L.reportTimer - 1
	local st = status()
	local rtt = st.rtt_ms
	local connection = st.connection or ''
	if connection ~= L.reportedConnection then
		pcall(nakama.lobbySend, 'member', {connection = connection})
		L.reportedConnection = connection
	end
	if rtt == nil or rtt < 0 then
		return
	end
	local changed = L.reportedPing == nil or pingLevel(rtt) ~= pingLevel(L.reportedPing)
	if changed or (L.reportTimer <= 0 and math.abs(rtt - L.reportedPing) >= 20) then
		pcall(nakama.lobbySend, 'member', {ping = rtt, region = st.region or '', connection = connection})
		L.reportedPing = rtt
		L.reportTimer = lobby.reportInterval
	end
end

-- Rules summary: format, rounds, time and the other rules that differ from
-- the defaults, joined with sep.
function lobby.f_rules(set, sep)
	local v = info().valuename or {}
	set = set or {}
	local parts = {}
	local format = v[set.format or 'queue'] or set.format or ''
	if set.format == 'winner_stays_on' and (v.streak_rule or '') ~= '' then
		format = format .. ' ' .. fmt(v.streak_rule, tonumber(set.max_games) or 3)
	end
	table.insert(parts, format)
	local rounds = tonumber(set.rounds) or 0
	if rounds > 0 and (v.rounds_rule or '') ~= '' then
		table.insert(parts, fmt(v.rounds_rule, rounds))
	end
	local time = tonumber(set.time) or 0
	if time > 0 and (v.time_rule or '') ~= '' then
		table.insert(parts, fmt(v.time_rule, time))
	elseif time < 0 and (v.infinite_rule or '') ~= '' then
		table.insert(parts, v.infinite_rule)
	end
	if set.teams == 'single' and (v.single_rule or '') ~= '' then
		table.insert(parts, v.single_rule)
	end
	if set.stage == 'random' and (v.random_rule or '') ~= '' then
		table.insert(parts, v.random_rule)
	end
	if set.private and (v.private_rule or '') ~= '' then
		table.insert(parts, v.private_rule)
	end
	if tonumber(set.watch) == 0 and (v.nowatch_rule or '') ~= '' then
		table.insert(parts, v.nowatch_rule)
	end
	return table.concat(parts, sep)
end

-- Waits for step() to return true or false while drawing the status
-- message key (the current screen is drawn by drawBehind, if given).
-- Returns that value, 'cancel' or 'timeout'.
local function waitFor(key, args, frames, step, drawBehind)
	local n = 0
	while frames == nil or n < frames do
		local r = step()
		if r ~= nil then
			return r
		end
		if esc() or pressed(info().browser.menu.cancel.key) then
			esc(false)
			snd(info().cancel.snd)
			return 'cancel'
		end
		drawBackground()
		if drawBehind ~= nil then
			drawBehind()
		end
		drawStatus(key, unpack(args or {}))
		drawForeground()
		refresh()
		n = n + 1
	end
	return 'timeout'
end

-- The server error of the last request, or text.
local function lastError(text)
	local st = status()
	if st.error ~= nil and st.error ~= '' then
		return st.error
	end
	return text
end

-- ---------------------------------------------------------------------------
-- Joining, creating and leaving

local function leaveLobby()
	online.f_endSession()
	if nakama ~= nil and nakama.currentMatch ~= nil and nakama.currentMatch() ~= '' then
		pcall(nakama.leaveMatch)
	end
	L.state = nil
	L.byId = {}
	L.me = nil
	L.kicked = false
end

-- Joins a lobby and waits for its first state. code is the room ID
-- (private lobbies need it). Returns true, or false and an error text.
function lobby.f_join(matchId, code)
	leaveLobby()
	nakama.clearError()
	local metadata = {}
	if code ~= nil and code ~= '' then
		metadata.code = code
	end
	local ok, err = pcall(nakama.joinMatch, matchId, nil, metadata)
	if not ok then
		return false, tostring(err)
	end
	local r = waitFor('joining', nil, lobby.requestFrames, function()
		local st = status()
		if not st.connected then
			return false
		end
		if st.error ~= nil and st.error ~= '' then
			return false
		end
		if st.match_id == matchId then
			lobby.f_poll()
			if L.state ~= nil then
				return true
			end
		end
	end)
	if r ~= true then
		local reason = lastError(r == 'timeout' and 'The lobby did not answer.' or nil)
		leaveLobby()
		return false, reason
	end
	L.chat = {}
	L.muted = {}
	L.handledSeq = L.state.current ~= nil and L.state.current.seq or -1
	L.reportedPing = nil
	L.pingTimer = 0
	L.reportTimer = 0
	L.reportedConnection = status().connection or ''
	pcall(nakama.lobbySend, 'member', {region = status().region or '', ping = -1, connection = L.reportedConnection})
	return true
end

-- Resolves a room ID and joins that lobby.
function lobby.f_joinCode(code)
	code = (code or ''):upper():gsub('[^%w]', '')
	if code == '' then
		return false
	end
	nakama.clearError()
	L.findReply = nil
	nakama.findLobby(code)
	local r = waitFor('finding', nil, lobby.requestFrames, function()
		if L.findReply ~= nil then
			return L.findReply.error == nil and L.findReply.match_id ~= nil
		end
		if not status().connected then
			return false
		end
	end)
	if r ~= true then
		return false, L.findReply and L.findReply.error or lastError(nil)
	end
	return lobby.f_join(L.findReply.match_id, code)
end

-- Creates a lobby with settings and joins it.
function lobby.f_create(settings)
	nakama.clearError()
	L.createReply = nil
	nakama.createLobby({settings = settings, name = settings.name, format = settings.format, max_games = settings.max_games})
	local r = waitFor('creating', nil, lobby.requestFrames, function()
		if L.createReply ~= nil then
			return L.createReply.error == nil and L.createReply.match_id ~= nil
		end
		if not status().connected then
			return false
		end
	end)
	if r ~= true then
		return false, L.createReply and L.createReply.error or lastError(nil)
	end
	local code = L.createReply.payload and L.createReply.payload.code or nil
	return lobby.f_join(L.createReply.match_id, code)
end

-- ---------------------------------------------------------------------------
-- Lobby settings

local function settingValue(name, s)
	local v = info().valuename or {}
	if name == 'name' then
		return s.name
	elseif name == 'comment' then
		if s.comment == nil or s.comment == '' then
			return v.none
		end
		return s.comment
	elseif name == 'size' then
		return fmt(v.size, s.size)
	elseif name == 'format' then
		return v[s.format] or s.format
	elseif name == 'maxgames' then
		return fmt(v.maxgames, s.max_games)
	elseif name == 'rounds' then
		if s.rounds == 0 then
			return v.default
		end
		return fmt(v.rounds, s.rounds)
	elseif name == 'time' then
		if s.time == 0 then
			return v.default
		elseif s.time < 0 then
			return v.infinite
		end
		return fmt(v.time, s.time)
	elseif name == 'teams' or name == 'stage' or name == 'start' then
		return v[s[name]] or s[name]
	elseif name == 'interval' then
		return fmt(v.interval, s.interval)
	elseif name == 'type' then
		return s.private and v.private or v.public
	elseif name == 'watch' then
		if (tonumber(s.watch) or 0) <= 0 then
			return v.off
		end
		return fmt(v.watch, s.watch)
	end
	return nil
end

local function cycle(name, s, dir)
	local list = lobby.choices[name]
	local field = FIELD[name]
	if list == nil or field == nil then
		return false
	end
	local index = 1
	for i, value in ipairs(list) do
		if value == s[field] then
			index = i
			break
		end
	end
	index = index + dir
	if index < 1 then
		index = #list
	elseif index > #list then
		index = 1
	end
	s[field] = list[index]
	return true
end

-- Lobby settings. mode is 'create' or 'edit'. Returns the chosen settings,
-- or nil when cancelled. tick() runs every frame (the room keeps playing)
-- and may return true to close the screen.
function lobby.f_settings(mode, current, tick)
	local si = info().settings
	local menu = si.menu
	local s = copy(current)
	local v = info().valuename or {}
	local m = newMenu(menu)
	local function rebuild()
		setMenuItems(m, menuItems(menu, function(name)
			if name == 'maxgames' then
				return s.format == 'winner_stays_on'
			end
			return true
		end, function(name, text)
			if name == 'confirm' and mode == 'edit' and (v.apply or '') ~= '' then
				return v.apply
			end
		end))
		for _, it in ipairs(m.items) do
			it.vardisplay = settingValue(it.itemname, s)
		end
	end
	rebuild()
	main.f_menuSnap(m.sec)
	local guard = lobby.inputGuardFrames
	resetKey()
	while true do
		if tick ~= nil and tick() then
			return nil
		end
		local name = m.items[m.item] and m.items[m.item].itemname or ''
		local addPressed, subtractPressed = false, false
		if guard == 0 then
			addPressed = pressed(menu.add.key)
			subtractPressed = not addPressed and pressed(menu.subtract.key)
		end
		if guard > 0 then
			guard = guard - 1
		elseif cancelPressed(menu) then
			snd(info().cancel.snd)
			return nil
		elseif addPressed or subtractPressed then
			-- getInput consumes a press, so each key is read once per frame.
			local dir = addPressed and 1 or -1
			if cycle(name, s, dir) then
				snd(info().cursor.move.snd)
				rebuild()
			end
		elseif pressed(menu.done.key) then
			if name == 'confirm' then
				snd(info().cursor.done.snd)
				if s.name == nil or s.name == '' then
					s.name = myName()
				end
				return s
			elseif name == 'back' then
				snd(info().cancel.snd)
				return nil
			elseif name == 'name' or name == 'comment' then
				snd(info().cursor.done.snd)
				local text = lobby.f_input(name, s[name], name == 'name' and 24 or 40, tick)
				if text ~= nil then
					s[name] = text
				end
				rebuild()
				guard = lobby.inputGuardFrames
			elseif cycle(name, s, 1) then
				snd(info().cursor.move.snd)
				rebuild()
			end
		else
			menuMove(m)
		end
		drawBackground()
		drawPanels(si.panel)
		drawText(si.title, si.title.text ~= nil and si.title.text[mode] or '')
		menuDraw(m)
		local infoText = si.info.text ~= nil and si.info.text[m.items[m.item] and m.items[m.item].itemname or ''] or ''
		drawText(si.info, infoText)
		drawForeground()
		refresh()
	end
end

-- ---------------------------------------------------------------------------
-- Online name

local function message(key, fallback)
	local m = info().message or {}
	local text = m[key]
	if text == nil or text == '' then
		return fallback or ''
	end
	return text
end

-- Asks for the player's online name (textinput.text.account, or
-- textinput.text.firstaccount when first is true) and saves it. A name the
-- lifebar cannot show (nakama.nameMissing) or the server refuses is asked
-- for again. Returns true when the name was saved.
function lobby.f_accountName(first)
	local name = ''
	if nakama.displayName ~= nil then
		name = nakama.displayName()
	end
	while true do
		local text = lobby.f_input(first and 'firstaccount' or 'account', name, lobby.nameLength[2])
		if text == nil then
			return false
		end
		text = text:gsub('^%s+', ''):gsub('%s+$', '')
		name = text
		local missing = ''
		if nakama.nameMissing ~= nil then
			missing = nakama.nameMissing(text)
		end
		if charCount(text) < lobby.nameLength[1] then
			warn(message('nameinvalid', 'A name has 2 to 16 characters.'))
		elseif missing ~= '' then
			warn(fmt(message('namefont', 'The lifebar cannot show: %s'), missing))
		else
			L.accountReply = nil
			nakama.clearError()
			if not pcall(nakama.setAccountName, text) then
				warn(message('namefailed', 'The name could not be changed.'))
				return false
			end
			-- A late reply to an earlier request names another name.
			local function answered()
				local r = L.accountReply
				return r ~= nil and (type(r.payload) ~= 'table' or r.payload.name == nil or r.payload.name == text)
			end
			local n = 0
			while not answered() and n < lobby.accountFrames do
				drawBackground()
				drawStatus('account')
				drawForeground()
				refresh()
				n = n + 1
			end
			local reply = nil
			if answered() then
				reply = L.accountReply
			end
			if reply ~= nil and reply.error == nil then
				snd(info().cursor.done.snd)
				return true
			end
			local reason, err = 'failed', ''
			if reply ~= nil then
				err = reply.error or ''
				if type(reply.payload) == 'table' and reply.payload.reason ~= nil then
					reason = reply.payload.reason
				end
			end
			if reason == 'invalid' then
				warn(message('nameinvalid', err))
			else
				warn(message('namefailed', err))
				return false
			end
		end
	end
end

-- First visit to lobby search in this session with this account: an account
-- whose name was never chosen (it has no display name) is asked for one.
-- Later visits only read the name again when the client lost it: a new
-- connection (nakama.connect) starts without it.
local function checkAccountName()
	local user = ''
	if nakama.userId ~= nil then
		user = nakama.userId()
	end
	if user == '' then
		return
	end
	if lobby.accountChecked == user then
		if nakama.displayName ~= nil and nakama.displayName() == '' then
			pcall(nakama.getAccount)
		end
		return
	end
	lobby.accountChecked = user
	L.accountInfo = nil
	if not pcall(nakama.getAccount) then
		return
	end
	local r = waitFor('connecting', nil, lobby.requestFrames, function()
		if L.accountInfo ~= nil then
			return L.accountInfo.error == nil
		end
		if not status().connected then
			return false
		end
	end)
	-- (Written without "x = cond and value or nil": gopher-lua miscompiles
	-- that form in some functions.)
	if r ~= true then
		return
	end
	local account = L.accountInfo.payload
	if type(account) == 'table' and (account.display_name or '') == '' then
		lobby.f_accountName(true)
	end
end

-- ---------------------------------------------------------------------------
-- Lobby search

local function parseLobbies(payload, filter)
	local out = {}
	local st = status()
	for _, m in ipairs(payload or {}) do
		local d = m.label_data
		if type(d) == 'table' and d.kind == 'ikemen-lobby' and d.visibility ~= 'private' and
			((d.game or '') == '' or d.game == (st.game or '')) and
			((d.build or '') == '' or d.build == (st.build or '')) then
			local entry = {
				match_id = m.match_id,
				name = d.name or '',
				comment = d.comment or '',
				code = d.code or '',
				host = d.host or '',
				hostTag = d.host_tag or '',
				region = d.region or '',
				players = tonumber(d.players) or tonumber(m.size) or 0,
				size = tonumber(d.size) or 8,
				phase = d.phase or 'waiting',
				open = d.open ~= 'no',
				members = d.members or {},
				tags = d.tags or {},
				settings = {
					format = d.format, max_games = d.max_games, rounds = d.rounds,
					time = d.time, teams = d.teams, stage = d.stage, watch = d.watch,
				},
			}
			local shown = true
			if filter == 'open' then
				shown = entry.open
			elseif filter == 'region' then
				shown = entry.open and entry.region ~= '' and entry.region == (st.region or '')
			end
			if shown then
				table.insert(out, entry)
			end
		end
	end
	table.sort(out, function(a, b)
		if a.open ~= b.open then
			return a.open
		end
		if (a.phase == 'waiting') ~= (b.phase == 'waiting') then
			return a.phase == 'waiting'
		end
		if a.players ~= b.players then
			return a.players > b.players
		end
		return a.name < b.name
	end)
	return out
end

local function drawBrowser(b, list, sel, top)
	local bi = info().browser
	local li = bi.list
	drawPanels(bi.panel)
	drawText(bi.title, bi.title.text)
	drawText(bi.count, fmt(bi.count.text, #list))
	drawText(bi.account, fmt(bi.account.text, myName()))
	drawText(li.header, li.header.text)
	local visible = math.max(1, li.visibleitems or 5)
	drawBox(li.box, li.pos[1], li.pos[2])
	if #list == 0 then
		drawText(li.empty, li.empty.text, li.pos[1], li.pos[2])
	end
	for row = 1, visible do
		local i = top + row - 1
		local entry = list[i]
		if entry == nil then
			break
		end
		local x = li.pos[1] + (row - 1) * li.spacing[1]
		local y = li.pos[2] + (row - 1) * li.spacing[2]
		local active = i == sel
		local rowEl = active and li.active or li.row
		drawElement(rowEl, x, y)
		-- Name and comment are cut off at the row box when it is visible.
		local c = rowEl.box.coords
		local window = rowEl.box.visible and {x + c[1], y + c[2], x + c[3], y + c[4] + 1} or nil
		drawText(li.name, fmt(li.name.text, entry.name), x, y, window)
		drawText(li.comment, fmt(li.comment.text, entry.comment), x, y, window)
		drawText(li.players, fmt(li.players.text, entry.players, entry.size), x, y)
		drawText(li.format, fmt(li.format.text, (info().valuename or {})[entry.settings.format or ''] or ''), x, y)
		drawText(li.code, fmt(li.code.text, entry.code), x, y)
		drawText(li.host, fmt(li.host.text, entry.host, entry.hostTag), x, y)
		local phase = li.phase.text ~= nil and li.phase.text[entry.phase] or ''
		if not entry.open and li.phase.text ~= nil and li.phase.text.full ~= nil then
			phase = li.phase.text.full
		end
		drawText(li.phase, phase, x, y)
	end
	if top > 1 then
		main.f_animPosDraw(li.arrow.up.AnimData)
	end
	if top + visible - 1 < #list then
		main.f_animPosDraw(li.arrow.down.AnimData)
	end
	local entry = list[sel]
	local d = bi.detail
	if entry ~= nil then
		local x, y = d.pos[1], d.pos[2]
		drawBox(d.box, x, y)
		local c = d.box.coords
		local window = d.box.visible and {x + c[1], y + c[2], x + c[3], y + c[4] + 1} or nil
		drawText(d.name, fmt(d.name.text, entry.name), x, y, window)
		drawText(d.comment, fmt(d.comment.text, entry.comment), x, y, window)
		drawText(d.code, fmt(d.code.text, entry.code), x, y, window)
		drawText(d.host, fmt(d.host.text, entry.host, entry.hostTag), x, y, window)
		drawText(d.rules, fmt(d.rules.text, lobby.f_rules(entry.settings, '\n')), x, y, window)
		for i, name in ipairs(entry.members) do
			drawText(d.member, fmt(d.member.text, name, entry.tags[i] or ''), x + (i - 1) * d.member.spacing[1], y + (i - 1) * d.member.spacing[2], window)
		end
	end
	menuDraw(b)
end

-- Lobby search. Returns when the player backs out.
function lobby.f_browser()
	local bi = info().browser
	local b = newMenu(bi.menu)
	local filter = 'all'
	local payload = {}
	local list = {}
	local sel, top = 1, 1
	local v = info().valuename or {}
	local function rebuild()
		setMenuItems(b, menuItems(bi.menu))
		for _, it in ipairs(b.items) do
			if it.itemname == 'filter' then
				it.vardisplay = v[filter] or filter
			end
		end
	end
	local function applyFilter()
		local current = list[sel] and list[sel].match_id
		list = parseLobbies(payload, filter)
		sel = 1
		for i, entry in ipairs(list) do
			if entry.match_id == current then
				sel = i
			end
		end
		top = 1
	end
	-- delay: frames to wait before asking the server (see lobby.listDelayFrames).
	local function search(delay)
		L.listReply = nil
		nakama.clearError()
		delay = delay or 0
		local sent = false
		local r = waitFor('searching', nil, delay + lobby.requestFrames, function()
			if not sent then
				if delay > 0 then
					delay = delay - 1
					return nil
				end
				sent = true
				if not pcall(nakama.listLobbies) then
					return false
				end
			end
			if L.listReply ~= nil then
				return L.listReply.error == nil
			end
			if not status().connected then
				return false
			end
		end, function()
			drawBrowser(b, list, sel, top)
		end)
		if r == true then
			payload = L.listReply.payload or {}
			applyFilter()
		elseif r ~= 'cancel' then
			warn(L.listReply and L.listReply.error or lastError(online.text.service))
		end
	end
	-- Returns false when the connection to the server is gone.
	local function enterRoom()
		local reason = lobby.f_room()
		leaveLobby()
		if reason == 'service' or not status().connected then
			return false
		end
		bgReset(motif.lobbybgdef.BGDef)
		fadeInInit(info().fadein.FadeData)
		playMusic(false)
		search(lobby.listDelayFrames)
		return true
	end

	if not online.f_ensureConnected(true, function(key)
		drawBackground()
		drawStatus('connecting')
		drawForeground()
		refresh()
	end) then
		warn(online.f_reasonText('service'))
		return
	end
	rebuild()
	main.f_menuSnap(b.sec)
	bgReset(motif.lobbybgdef.BGDef)
	fadeInInit(info().fadein.FadeData)
	playMusic(false)
	checkAccountName()
	if not status().connected then
		warn(online.f_reasonText('service'))
		return
	end
	search()
	local guard = lobby.inputGuardFrames
	-- Backing out fades out (fadeout) before returning to the title menu.
	local closing = false
	local function close()
		snd(info().cancel.snd)
		fadeOutInit(info().fadeout.FadeData)
		closing = true
	end
	while true do
		local visible = math.max(1, bi.list.visibleitems or 5)
		if not status().connected then
			warn(online.f_reasonText('service'))
			break
		end
		if closing then
			if not fadeActive() then
				break
			end
		elseif guard > 0 then
			guard = guard - 1
		elseif fadeActive() then
			if esc() or pressed(bi.menu.cancel.key) then
				fadeSkip()
			end
		elseif cancelPressed(bi.menu) then
			close()
		elseif pressed(bi.list.next.key) then
			if sel < #list then
				sel = sel + 1
				snd(info().cursor.move.snd)
			end
		elseif pressed(bi.list.previous.key) then
			if sel > 1 then
				sel = sel - 1
				snd(info().cursor.move.snd)
			end
		elseif pressed(bi.menu.done.key) then
			local name = b.items[b.item] and b.items[b.item].itemname or ''
			if name == 'back' then
				close()
			else
				snd(info().cursor.done.snd)
			end
			if name == 'join' then
				local entry = list[sel]
				if entry ~= nil then
					local ok, err = lobby.f_join(entry.match_id, entry.code)
					if ok then
						if not enterRoom() then
							break
						end
					else
						warn(err or online.text.service)
						search()
					end
				end
			elseif name == 'search' then
				search()
			elseif name == 'create' then
				local defaults = copy(lobby.lastSettings or lobby.defaultSettings)
				if defaults.name == nil or defaults.name == '' then
					defaults.name = myName()
				end
				local settings = lobby.f_settings('create', defaults)
				if settings ~= nil then
					lobby.lastSettings = copy(settings)
					local ok, err = lobby.f_create(settings)
					if ok then
						if not enterRoom() then
							break
						end
					elseif err ~= nil then
						warn(err)
					end
				end
			elseif name == 'code' then
				local code = lobby.f_input('code', '', 12)
				if code ~= nil and code ~= '' then
					local ok, err = lobby.f_joinCode(code)
					if ok then
						if not enterRoom() then
							break
						end
					elseif err ~= nil then
						warn(err)
					end
				end
			elseif name == 'filter' then
				-- all -> open (not full) -> region (open, host in my region)
				filter = ({all = 'open', open = 'region', region = 'all'})[filter] or 'all'
				rebuild()
				applyFilter()
			elseif name == 'account' then
				lobby.f_accountName(false)
				if not status().connected then
					warn(online.f_reasonText('service'))
					break
				end
			end
			guard = lobby.inputGuardFrames
		else
			menuMove(b)
		end
		if sel > #list then
			sel = math.max(1, #list)
		end
		if sel < top then
			top = sel
		elseif sel > top + visible - 1 then
			top = sel - visible + 1
		end
		drawBackground()
		drawBrowser(b, list, sel, top)
		drawForeground()
		refresh()
	end
	leaveLobby()
end

-- ---------------------------------------------------------------------------
-- Lobby room

local function slotPos(i)
	local sl = info().room.slot
	local cols = math.max(1, sl.columns or 1)
	local col = (i - 1) % cols
	local row = math.floor((i - 1) / cols)
	-- Columns advance by spacing x, rows by spacing y.
	return sl.pos[1] + col * sl.spacing[1], sl.pos[2] + row * sl.spacing[2]
end

-- Connection bars of the ping element p for a round trip in ms. direct: the
-- round trip was measured over the players' direct connection, which
-- draws p.direct too and colours lit bars with p.directon when it is set.
local function drawPing(p, x, y, ping, direct)
	local level = pingLevel(ping, p)
	if level == nil then
		drawText(p.unknown, p.unknown.text, x, y)
		return
	end
	local art = p.level ~= nil and p.level[tostring(level)] or nil
	if art ~= nil and art.AnimData ~= nil then
		main.f_animPosDraw(art.AnimData, x, y)
	else
		local on = p.on
		if direct and p.directon ~= nil and p.directon.visible then
			on = p.directon
		end
		for bar = 1, math.max(0, p.bars or 0) do
			local box = bar <= level and on or p.off
			local bx = x + p.offset[1] + (bar - 1) * p.spacing[1]
			local by = y + p.offset[2] + (bar - 1) * p.spacing[2]
			drawBox(box, bx, by, (bar - 1) * p.grow[1], (bar - 1) * p.grow[2])
		end
	end
	drawText(p.text, fmt(p.text.text, ping), x, y)
	if direct then
		drawElement(p.direct, x, y, nil, ping)
	end
end

local function drawSlots(cursorIndex)
	local sl = info().room.slot
	local s = L.state
	local members = s and s.members or {}
	local size = s and s.settings and s.settings.size or 8
	local me = nakama.userId()
	local count = math.max(8, #members)
	for i = 1, count do
		local x, y = slotPos(i)
		local m = members[i]
		local kind = 'member'
		if m == nil then
			kind = i <= size and 'empty' or 'closed'
		elseif m.user_id == me then
			kind = 'self'
		end
		local bg = sl.bg[kind] or (kind == 'self' and sl.bg.member) or sl.bg.default
		drawElement(bg, x, y)
		if cursorIndex == i then
			drawElement(sl.cursor, x, y)
		end
		drawText(sl.number, fmt(sl.number.text, i), x, y)
		if m == nil then
			if kind == 'empty' then
				drawText(sl.empty, sl.empty.text, x, y)
			else
				drawText(sl.closed, sl.closed.text, x, y)
			end
		else
			drawText(sl.name, fmt(sl.name.text, memberName(m), tagOf(m.user_id)), x, y)
			drawText(sl.tag, fmt(sl.tag.text, tagOf(m.user_id)), x, y)
			if m.host then
				drawElement(sl.host, x, y)
			end
			if m.user_id == me then
				drawElement(sl.self, x, y)
			end
			local region = m.region or ''
			local flag = region ~= '' and sl.flag ~= nil and sl.flag[region:lower()] or nil
			if flag ~= nil and flag.AnimData ~= nil then
				main.f_animPosDraw(flag.AnimData, x, y)
			elseif region ~= '' then
				drawText(sl.region, fmt(sl.region.text, region), x, y)
			end
			if sl.connection ~= nil and sl.connection[m.connection or ''] ~= nil then
				drawElement(sl.connection[m.connection or ''], x, y)
			end
			-- A member this player has played shows the round trip measured
			-- over their direct connection; the others their round trip to
			-- the server.
			local direct = nil
			if m.user_id ~= me and L.me ~= nil and type(L.me.links) == 'table' then
				direct = tonumber(L.me.links[m.user_id])
			end
			if direct ~= nil then
				drawPing(sl.ping, x, y, direct, true)
			else
				drawPing(sl.ping, x, y, m.ping)
			end
			drawText(sl.record, fmt(sl.record.text, m.wins or 0, m.losses or 0), x, y)
			local badge = sl.badge[m.state or 'standby'] or sl.badge.default
			if badge ~= nil then
				drawElement(badge, x, y)
			end
		end
	end
end

-- Splits a word wider than width into pieces that fit, between UTF-8
-- characters: text without spaces, such as Chinese or Japanese, and long
-- words. Every piece has at least one character.
local function splitWord(el, word, width)
	local pieces, piece = {}, ''
	local i, n = 1, #word
	while i <= n do
		local c = word:byte(i)
		local size = 1
		if c >= 0xF0 then
			size = 4
		elseif c >= 0xE0 then
			size = 3
		elseif c >= 0xC0 then
			size = 2
		end
		local ch = word:sub(i, i + size - 1)
		if piece ~= '' and textWidth(el, piece .. ch) > width then
			table.insert(pieces, piece)
			piece = ch
		else
			piece = piece .. ch
		end
		i = i + size
	end
	if piece ~= '' then
		table.insert(pieces, piece)
	end
	return pieces
end

-- Splits text into lines no wider than width when drawn with el: at spaces
-- and line breaks, and inside a word too wide for a line of its own.
local function wrapText(el, text, width)
	local lines = {}
	for paragraph in (text .. '\n'):gmatch('(.-)\n') do
		local line = ''
		for word in paragraph:gmatch('%S+') do
			local candidate = word
			if line ~= '' then
				candidate = line .. ' ' .. word
			end
			if textWidth(el, candidate) <= width then
				line = candidate
			else
				if line ~= '' then
					table.insert(lines, line)
					line = ''
				end
				if textWidth(el, word) <= width then
					line = word
				else
					local pieces = splitWord(el, word, width)
					for k = 1, #pieces - 1 do
						table.insert(lines, pieces[k])
					end
					line = pieces[#pieces] or ''
				end
			end
		end
		table.insert(lines, line)
	end
	return lines
end

-- Chat log rows: long messages wrap within the chat box and continue under
-- the message text.
local function chatRows(c)
	local rows = {}
	local right = c.box.visible and (c.box.coords[3] - 4) or 300
	local lines = math.max(0, c.lines or 0)
	local first = math.max(1, #L.chat - lines + 1)
	for i = first, #L.chat do
		local line = L.chat[i]
		if line.kind == 'chat' then
			local name = fmt(c.name.text, line.name, line.tag or '')
			local indent = textWidth(c.name, name) + (c.text.offset ~= nil and c.text.offset[1] or 0)
			if right - indent < right / 3 then
				-- A name too wide to leave room beside it: the message
				-- starts on the next row.
				table.insert(rows, {el = c.text, text = '', name = name, indent = 0})
				for _, text in ipairs(wrapText(c.text, line.text, right)) do
					table.insert(rows, {el = c.text, text = text, indent = 0})
				end
			else
				for n, text in ipairs(wrapText(c.text, line.text, right - indent)) do
					table.insert(rows, {el = c.text, text = text, name = n == 1 and name or nil, indent = textWidth(c.name, name)})
				end
			end
		else
			local el, text = c.error, line.text
			if line.kind == 'notice' then
				el = c.notice
				local pattern = c.notice.text ~= nil and c.notice.text[line.notice] or nil
				text = (pattern ~= nil and pattern ~= '') and fmt(pattern, line.name, line.tag or '') or nil
			end
			if text ~= nil and text ~= '' then
				for _, part in ipairs(wrapText(el, text, right)) do
					table.insert(rows, {el = el, text = part, indent = 0})
				end
			end
		end
	end
	return rows
end

local function drawChat(entry)
	local c = info().room.chat
	local x0, y0 = c.pos[1], c.pos[2]
	drawBox(c.box, x0, y0)
	local rows = chatRows(c)
	local lines = math.max(0, c.lines or 0)
	local first = math.max(1, #rows - lines + 1)
	for i = first, #rows do
		local row = rows[i]
		local n = i - first
		local x = x0 + n * c.spacing[1]
		local y = y0 + n * c.spacing[2]
		if row.name ~= nil then
			drawText(c.name, row.name, x, y)
		end
		drawText(row.el, row.text, x + row.indent, y)
	end
	if entry ~= nil then
		drawBox(c.input.box, x0 + c.input.offset[1], y0 + c.input.offset[2])
		-- Long input shows its end, so the caret stays in view.
		local right = c.box.visible and (c.box.coords[3] - 4) or 300
		local text = entry.text
		local shown = fmt(c.input.text, text)
		while text ~= '' and textWidth(c.input, shown) > right - c.input.offset[1] do
			text = text:sub(2)
			while text ~= '' and text:byte(1) >= 0x80 and text:byte(1) < 0xC0 do
				text = text:sub(2)
			end
			shown = fmt(c.input.text, text)
		end
		drawText(c.input, shown, x0, y0)
	end
end

-- The status line, the in-game panel or the between-matches panel.
local function drawPhase()
	local r = info().room
	local s = L.state
	if s == nil then
		return
	end
	local texts = r.status.text or {}
	if s.phase == 'playing' and s.current ~= nil then
		local mt = r.match
		local x, y = mt.pos[1], mt.pos[2]
		drawElement(mt.bg, x, y)
		drawText(mt.title, mt.title.text, x, y)
		drawText(mt.p1, fmt(mt.p1.text, s.current.player1_name or nameOf(s.current.player1)), x, y)
		drawText(mt.vs, mt.vs.text, x, y)
		drawText(mt.p2, fmt(mt.p2.text, s.current.player2_name or nameOf(s.current.player2)), x, y)
		local elapsed = matchElapsed()
		drawText(mt.time, fmt(mt.time.text, math.floor(elapsed / 60), elapsed % 60), x, y)
		if s.streak ~= nil and (s.streak.count or 0) > 0 then
			drawText(mt.streak, fmt(mt.streak.text, s.streak.count, nameOf(s.streak.user_id)), x, y)
		end
		-- The players' round trip over their direct connection.
		if tonumber(s.current.link) ~= nil then
			drawPing(mt.link, x, y, tonumber(s.current.link), true)
		end
		return
	end
	local finished = s.phase == 'finished' or (s.tournament ~= nil and s.tournament.finished == true)
	if s.phase == 'results' and s.last_result ~= nil then
		local rs = r.results
		local res = s.last_result
		local x, y = rs.pos[1], rs.pos[2]
		drawElement(rs.bg, x, y)
		local titles = rs.title.text or {}
		if res.no_contest then
			drawText(rs.title, titles.nocontest or '', x, y)
			local reasons = rs.reason.text or {}
			local text = reasons[res.reason or ''] or reasons.default or ''
			drawText(rs.reason, fmt(text, res.player1_name or '', res.player2_name or ''), x, y)
		else
			-- The final of a tournament is titled text.finished.
			local title = titles.win or ''
			if finished and (titles.finished or '') ~= '' then
				title = titles.finished
			end
			drawText(rs.title, title, x, y)
			drawText(rs.winner, fmt(rs.winner.text, res.winner_name or ''), x, y)
			drawText(rs.loser, fmt(rs.loser.text, res.loser_name or ''), x, y)
			if (res.streak or 0) > 1 then
				drawText(rs.streak, fmt(rs.streak.text, res.streak), x, y)
			end
		end
		drawText(rs.timer, fmt(rs.timer.text, phaseRemaining()), x, y)
		if s.next ~= nil then
			drawText(rs.next, fmt(rs.next.text, s.next.player1_name or '', s.next.player2_name or ''), x, y)
			drawText(r.status, fmt(texts.results, phaseRemaining()))
			return
		end
		-- No pairing is announced: the status line says what the lobby waits for.
	end
	local text = nil
	local set = s.settings or {}
	if finished then
		local t = s.tournament or {}
		local winner = t.winner_name or nameOf(t.winner)
		if winner == '' and type(t.tied_names) == 'table' and #t.tied_names > 0 then
			-- A round robin in which several entrants share the most wins.
			local v = info().valuename or {}
			text = fmt(texts.finishedtie, table.concat(t.tied_names, (v.separator or ',') .. ' '))
		else
			text = fmt(texts.finished, winner)
		end
	elseif isQueueFormat(set.format) then
		if not s.started then
			text = texts.waithost
		elseif L.me ~= nil and not L.me.ready then
			text = texts.notready
		else
			text = texts.waiting
		end
	else
		local t = s.tournament or {}
		if not t.started then
			text = set.start == 'host' and texts.waithost or texts.waitall
		elseif s.next ~= nil then
			local waitingFor = nil
			for _, id in ipairs({s.next.player1, s.next.player2}) do
				local m = L.byId[id]
				if m ~= nil and not m.ready then
					waitingFor = memberName(m)
				end
			end
			if waitingFor ~= nil then
				text = fmt(texts.waitentrant, waitingFor)
			end
		end
	end
	if text ~= nil then
		drawText(r.status, text)
	end
end

local function drawRoom(m, focus, cursorIndex, entry)
	local r = info().room
	local s = L.state
	if s == nil then
		return
	end
	local set = s.settings or {}
	drawPanels(r.panel)
	drawText(r.title, fmt(r.title.text, set.name or ''))
	drawText(r.comment, fmt(r.comment.text, set.comment or ''))
	drawText(r.code, fmt(r.code.text, s.code or ''))
	drawText(r.count, fmt(r.count.text, #(s.members or {}), set.size or 8))
	local v = info().valuename or {}
	drawText(r.rules, fmt(r.rules.text, lobby.f_rules(set, (v.separator or ',') .. ' ')))
	drawSlots(cursorIndex)
	drawPhase()
	if m ~= nil then
		menuDraw(m, focus == 'players' or focus == 'chat')
	end
	drawChat(entry)
end

-- True when the room has to close: removed, lobby gone, connection lost.
local function roomClosed()
	if L.kicked then
		return 'kicked'
	end
	local st = status()
	if not st.connected then
		return 'service'
	end
	if st.match_id == '' or L.state == nil then
		return 'gone'
	end
	return nil
end

-- Plays this player's match: P2P session, the fight, then the result.
function lobby.f_playMatch()
	local s = L.state
	local cur = s.current
	L.handledSeq = cur.seq
	local me = nakama.userId()
	local host = cur.player1 == me
	local peer = host and cur.player2 or cur.player1
	local peerName = (host and cur.player2_name or cur.player1_name) or nameOf(peer)
	local settings = copy(s.settings)
	eventSnd('match')
	pcall(nakama.setPairing, peer, host)
	-- The fight screen shows each player's online name in place of the name
	-- of the character they play as; the stream carries the names to the
	-- members who watch (f_publishSelection).
	lobby.matchNames = {hudName(cur.player1), hudName(cur.player2)}
	online.f_clearMatchNames()
	pcall(nakama.setMatchNames, lobby.matchNames[1], lobby.matchNames[2], cur.player1, cur.player2)
	-- The match replay names the lobby and the players' accounts (replay.lua).
	local accounts = {}
	for _, uid in ipairs({cur.player1, cur.player2}) do
		local m = L.byId[uid]
		if m ~= nil then
			accounts[uid] = {username = m.username or '', display_name = m.display_name or ''}
		end
	end
	replay.context = {kind = 'lobby', lobby = {name = settings.name or '', code = s.code or '', seq = cur.seq},
		accounts = accounts}
	-- The pairing's host publishes the match for the members who watch it,
	-- when the lobby allows it; lobby_match.lua adds the selection.
	local watch = tonumber(settings.watch) or 0
	lobby.matchSeq = cur.seq
	pcall(nakama.setReplayPublish, {enabled = host and watch > 0, delay = watch, info = {kind = 'lobby', seq = cur.seq}})
	local ok, reason = online.f_openSession(true, host, function(key)
		-- The room stays current behind the message. When the pairing ends
		-- meanwhile (the other player backed out or reported a failure),
		-- stop waiting for a player who is no longer coming.
		lobby.f_poll()
		local now = L.state and L.state.current
		if now == nil or now.seq ~= cur.seq then
			return 'ended'
		end
		drawBackground()
		drawRoom(nil, 'menu', nil, nil)
		drawStatus(key == 'session' and 'session' or 'pairing', peerName)
		drawForeground()
		refresh()
	end)
	if not ok then
		if reason ~= 'ended' then
			pcall(nakama.lobbySend, 'pair_failed', {seq = cur.seq, reason = reason or 'p2p'})
		end
		online.f_endSession()
		-- A cancelled or already ended pairing needs no explanation here: the
		-- results panel tells every member (room.results.reason.text.*).
		if reason ~= 'cancel' and reason ~= 'ended' then
			addChat({kind = 'error', text = online.f_reasonText(reason or 'p2p')})
		end
		pcall(nakama.setReplayPublish)
		online.f_clearMatchNames()
		replay.context = nil
		esc(false)
		return
	end
	local winner = nil
	-- synchronize() raises an error for failures that are not session
	-- warnings; the player stays in the lobby either way.
	local okSync, synced = pcall(synchronize)
	if not okSync then
		addChat({kind = 'error', text = tostring(synced)})
		synced = false
	end
	if synced then
		main.f_clearShuffleTables()
		lobby.f_setupMatch(settings)
		lobby.matchWinner = nil
		start.f_selectMode()
		winner = lobby.matchWinner
	end
	replayStop()
	exitNetPlay()
	exitReplay()
	online.f_endSession()
	pcall(nakama.setReplayPublish)
	online.f_clearMatchNames()
	replay.context = nil
	local warning = getSessionWarning()
	if warning ~= nil and warning ~= '' then
		addChat({kind = 'error', text = warning})
	end
	if winner == 1 or winner == 2 then
		local winnerId = winner == 1 and cur.player1 or cur.player2
		local loserId = winner == 1 and cur.player2 or cur.player1
		pcall(nakama.reportLobbyResult, winnerId, loserId, cur.seq)
	elseif not synced then
		-- The two games could not agree on a session (different content or
		-- settings, see the warning above): like a failed connection, the
		-- lobby should not pair these two players again.
		pcall(nakama.lobbySend, 'pair_failed', {seq = cur.seq, reason = 'session_failed'})
	else
		pcall(nakama.lobbySend, 'no_contest', {seq = cur.seq, reason = winner == 0 and 'draw' or 'aborted'})
	end
	main.f_default()
	esc(false)
	resetKey()
	bgReset(motif.lobbybgdef.BGDef)
	fadeInInit(info().fadein.FadeData)
	playMusic(true)
end

-- Versus rules for a lobby match, applied the same way on both machines
-- from the lobby's settings (they cannot change while a match is played).
function lobby.f_setupMatch(set)
	main.f_default()
	main.cpuSide[2] = false
	main.motif.vsscreen = true
	main.motif.victoryscreen = true
	main.orderSelect[1] = true
	main.orderSelect[2] = true
	main.pauseMenu = false
	main.selectMenu[2] = true
	main.stageMenu = set.stage ~= 'random'
	local single = set.teams == 'single'
	for side = 1, 2 do
		main.teamMenu[side].single = true
		main.teamMenu[side].simul = not single
		main.teamMenu[side].tag = not single
		main.teamMenu[side].turns = not single
	end
	local rounds = tonumber(set.rounds) or 0
	if rounds > 0 then
		main.matchWins.single = {rounds, rounds}
		main.matchWins.simul = {rounds, rounds}
		main.matchWins.tag = {rounds, rounds}
	end
	local time = tonumber(set.time) or 0
	if time ~= 0 then
		main.roundTime = time
	end
	local title = motif.select_info.title.text.netplaylobby or motif.select_info.title.text.netplayversus
	textImgSetText(motif.select_info.title.TextSpriteData, title or '')
	setGameMode('netplaylobby')
	setHomeTeam(1)
	-- One match, then back to the room (external/script/lobby_match.lua).
	main.luaPath = 'external/script/lobby_match.lua'
	hook.run('lobby.f_setupMatch', set)
end

-- The fight about to start, as a spectator loads it: character refs and
-- palettes in fight order (after order select) and team modes
-- (replay.f_selection). lobby_match.lua sends it with the replay stream
-- (nakama.setReplayInfo); the stream's header names the stage.
function lobby.f_selection()
	return replay.f_selection()
end

function lobby.f_publishSelection()
	if lobby.matchSeq == nil then
		return
	end
	pcall(nakama.setReplayInfo, {kind = 'lobby', seq = lobby.matchSeq, selection = lobby.f_selection(),
		names = lobby.matchNames or {'', ''}})
end

-- The stream of the pairing seq when this client has it: nakama.replayStatus()
-- with match_info from the lobby screens of the publishing player.
local function streamOf(seq)
	if nakama.replayStatus == nil then
		return nil
	end
	local ok, st = pcall(nakama.replayStatus)
	if not ok or type(st) ~= 'table' then
		return nil
	end
	local mi = st.match_info
	if type(mi) ~= 'table' or mi.kind ~= 'lobby' or tonumber(mi.seq) ~= seq then
		return nil
	end
	return st
end

-- Watches the current match: waits until its stream can play (asking the
-- lobby for it from the start when this client missed its beginning),
-- loads the same fight and plays it from the players' inputs. Esc stops
-- watching. Returns when the watched match (or watching) ends.
function lobby.f_watchMatch()
	local cur = L.state and L.state.current
	if cur == nil then
		return
	end
	local seq = cur.seq
	if streamOf(seq) == nil then
		pcall(nakama.lobbySend, 'watch', {seq = seq})
	end
	pcall(nakama.lobbySend, 'watching', {watching = true})
	local function stop(key)
		pcall(nakama.lobbySend, 'watching', {watching = false})
		if key ~= nil then
			warn(message(key))
		end
	end
	local r = waitFor('watching', {cur.player1_name or '', cur.player2_name or ''}, lobby.watchFrames, function()
		lobby.f_poll()
		lobby.f_tickPing()
		if roomClosed() ~= nil then
			return false
		end
		local st = streamOf(seq)
		if st ~= nil then
			if st.reset_reason ~= nil then
				return false
			end
			if st.ready then
				return true
			end
		end
		local now = L.state and L.state.current
		if (now == nil or now.seq ~= seq) and (st == nil or not st.ended) then
			return false
		end
	end, function()
		drawRoom(nil, 'menu', nil, nil)
	end)
	if r ~= true then
		if r ~= 'cancel' and roomClosed() == nil then
			stop('watchfailed')
		else
			stop()
		end
		return
	end
	local stream = streamOf(seq)
	local selection = stream.match_info.selection
	if type(selection) ~= 'table' or type(selection.p1) ~= 'table' or type(selection.p2) ~= 'table' or
		#selection.p1 == 0 or #selection.p2 == 0 or (stream.stage or '') == '' then
		stop('watchfailed')
		return
	end
	-- The players' settings first (the host's game options, in the stream
	-- header), then the lobby's rules on top, as on the players' machines.
	-- The stream is ready (checked above); wait = false keeps the engine's
	-- own spectator loading screen out if that changed meanwhile.
	local ok, started = pcall(nakama.startSpectatorReplay, {wait = false})
	if not ok or started ~= true then
		exitReplay()
		print('Lobby watch: ' .. tostring(started))
		if not ok and tostring(started):find('mismatch', 1, true) then
			stop('watchcontent')
		else
			stop('watchfailed')
		end
		return
	end
	-- The players' online names, as their fight screens show them.
	local names = stream.match_info.names
	online.f_clearMatchNames()
	if type(names) == 'table' then
		pcall(nakama.setMatchNames, tostring(names[1] or ''), tostring(names[2] or ''))
	end
	local settings = copy(L.state and L.state.settings or {})
	lobby.f_setupMatch(settings)
	start.f_selectReset(true)
	main.t_availableChars = main.f_tableCopy(start.f_getOrderChars())
	start.t_roster = {}
	-- Replay inputs are read per side: controller 1 is player 1.
	resetRemapInput()
	main.f_saveBaseRemapInput()
	local winner = nil
	local game = start.f_game
	start.f_game = function(common)
		winner = game(common)
		return winner
	end
	local okFight, err = pcall(launchFight, {
		p1char = selection.p1,
		p2char = selection.p2,
		p1teammode = selection.p1teammode,
		p2teammode = selection.p2teammode,
		p1numchars = #selection.p1,
		p2numchars = #selection.p2,
		stage = stream.stage,
		forceStage = true,
		vsscreen = false,
		p1orderselect = false,
		p2orderselect = false,
		continue = false,
		quickcontinue = true,
		victoryscreen = false,
		winscreen = false,
	})
	start.f_game = game
	if not okFight then
		print('Lobby watch: ' .. tostring(err))
	end
	print('Lobby watch: match ' .. tostring(seq) .. ' winner ' .. tostring(winner))
	online.f_clearMatchNames()
	exitReplay()
	setMatchNo(-1)
	main.f_default()
	stop()
	esc(false)
	resetKey()
	bgReset(motif.lobbybgdef.BGDef)
	fadeInInit(info().fadein.FadeData)
	playMusic(true)
end

local function roomMenuItems(m)
	local s = L.state
	local set = s and s.settings or {}
	local host = isHost()
	local v = m.sec.menu.valuename or {}
	setMenuItems(m, menuItems(m.sec.menu, function(name)
		if name == 'settings' then
			if not host or s == nil then
				return false
			end
			if s.phase == 'playing' or s.phase == 'results' then
				return false
			end
			return isQueueFormat(set.format) or s.phase == 'finished' or not (s.tournament and s.tournament.started)
		elseif name == 'start' then
			if not host or s == nil then
				return false
			end
			if isQueueFormat(set.format) then
				return set.start == 'host' and not s.started
			end
			local t = s.tournament or {}
			return not t.started or t.finished
		elseif name == 'watch' then
			return lobby.f_canWatch()
		end
		return true
	end, function(name, text)
		if name == 'ready' and L.me ~= nil and L.me.ready and (v.unready or '') ~= '' then
			return v.unready
		end
	end))
end

-- True when this player can watch the match being played: the lobby allows
-- it, the players' stream has started, and this player is not one of them.
function lobby.f_canWatch()
	local s = L.state
	local cur = s and s.current
	if s == nil or s.phase ~= 'playing' or cur == nil or not cur.watch then
		return false
	end
	if (tonumber(s.settings and s.settings.watch) or 0) <= 0 then
		return false
	end
	local me = nakama.userId()
	return cur.player1 ~= me and cur.player2 ~= me
end

local function playerMenuItems(pm, target)
	local host = isHost()
	local me = nakama.userId()
	local v = pm.sec.menu.valuename or {}
	setMenuItems(pm, menuItems(pm.sec.menu, function(name)
		if name == 'kick' or name == 'host' then
			return host and target ~= me
		elseif name == 'mute' then
			return target ~= me
		end
		return true
	end, function(name, text)
		if name == 'mute' and L.muted[target] and (v.unmute or '') ~= '' then
			return v.unmute
		end
	end))
end

-- The lobby room. Returns when the player leaves or the lobby ends: nil,
-- or the roomClosed() reason ('kicked', 'service', 'gone').
function lobby.f_room()
	local r = info().room
	local m = newMenu(r.menu)
	local pm = newMenu(r.playermenu)
	local focus = 'menu'
	local cursorIndex = 1
	local target = nil
	local entry = nil
	local guard = lobby.inputGuardFrames
	local signature = nil
	bgReset(motif.lobbybgdef.BGDef)
	fadeInInit(info().fadein.FadeData)
	playMusic(false)
	resetKey()
	local function tick()
		lobby.f_poll()
		lobby.f_tickPing()
		return roomClosed() ~= nil or myPairingPending()
	end
	while true do
		lobby.f_poll()
		local closed = roomClosed()
		if closed ~= nil then
			if closed == 'kicked' then
				warn(r.status.text ~= nil and r.status.text.kicked or 'You were removed from the lobby.')
			elseif closed == 'service' then
				warn(online.f_reasonText('service'))
			else
				warn(r.status.text ~= nil and r.status.text.closed or 'The lobby has closed.')
			end
			return closed
		end
		lobby.f_tickPing()
		if myPairingPending() then
			entry = nil
			focus = 'menu'
			lobby.f_playMatch()
			guard = lobby.afterMatchGuardFrames
			signature = nil
		else
			-- Rebuild the menus when what they depend on changes.
			local s = L.state
			local sig = table.concat({tostring(isHost()), s.phase, tostring(s.started), tostring(L.me and L.me.ready),
				tostring(s.settings and s.settings.start), tostring(s.tournament and s.tournament.started),
				tostring(s.tournament and s.tournament.finished), tostring(s.settings and s.settings.format),
				tostring(lobby.f_canWatch())}, '|')
			if sig ~= signature then
				signature = sig
				roomMenuItems(m)
				if focus == 'playermenu' then
					playerMenuItems(pm, target)
				end
			end
			local members = s.members or {}
			if focus == 'players' and cursorIndex > #members then
				cursorIndex = math.max(1, #members)
			end
			if guard > 0 then
				guard = guard - 1
			elseif focus == 'chat' then
				local done = editText(entry, lobby.chatLimit)
				if done ~= nil then
					if done and entry.text ~= '' then
						pcall(nakama.lobbySend, 'chat', {text = entry.text})
					end
					entry = nil
					focus = 'menu'
					guard = lobby.inputGuardFrames
				end
			elseif fadeActive() then
				if esc() or pressed(r.menu.cancel.key) then
					fadeSkip()
				end
			elseif focus == 'players' then
				if cancelPressed(r.menu) then
					snd(info().cancel.snd)
					focus = 'menu'
				elseif pressed(r.menu.next.key) then
					if cursorIndex < #members then
						cursorIndex = cursorIndex + 1
						snd(info().cursor.move.snd)
					end
				elseif pressed(r.menu.previous.key) then
					if cursorIndex > 1 then
						cursorIndex = cursorIndex - 1
						snd(info().cursor.move.snd)
					end
				elseif pressed(r.menu.done.key) and members[cursorIndex] ~= nil then
					snd(info().cursor.done.snd)
					target = members[cursorIndex].user_id
					playerMenuItems(pm, target)
					pm.item, pm.cursorPosY = 1, 1
					focus = 'playermenu'
				end
			elseif focus == 'playermenu' then
				if cancelPressed(r.playermenu) then
					snd(info().cancel.snd)
					focus = 'players'
				elseif pressed(r.playermenu.done.key) then
					local name = pm.items[pm.item] and pm.items[pm.item].itemname or ''
					snd(info().cursor.done.snd)
					if name == 'mute' then
						L.muted[target] = not L.muted[target] or nil
					elseif name == 'kick' then
						pcall(nakama.lobbySend, 'kick', {user_id = target})
					elseif name == 'host' then
						pcall(nakama.lobbySend, 'host', {user_id = target})
					end
					focus = 'players'
				else
					menuMove(pm)
				end
			else
				if cancelPressed(r.menu) then
					local name = m.items[m.item] and m.items[m.item].itemname or ''
					snd(info().cancel.snd)
					-- Cancel moves the cursor to LEAVE, and leaves from there. A
					-- screenpack that hides LEAVE leaves on the first cancel.
					local hasLeave = false
					for _, it in ipairs(m.items) do
						if it.itemname == 'leave' then
							hasLeave = true
						end
					end
					if name == 'leave' or not hasLeave then
						return
					end
					selectMenuItem(m, 'leave')
				elseif pressed(r.menu.done.key) then
					local name = m.items[m.item] and m.items[m.item].itemname or ''
					if name == 'leave' then
						snd(info().cancel.snd)
						return
					end
					snd(info().cursor.done.snd)
					if name == 'ready' then
						local ready = not (L.me ~= nil and L.me.ready)
						pcall(nakama.lobbySend, 'ready', {ready = ready})
						if ready then
							eventSnd('ready')
						end
					elseif name == 'chat' then
						entry = {text = ''}
						focus = 'chat'
						resetKey()
					elseif name == 'settings' then
						local new = lobby.f_settings('edit', s.settings, tick)
						if new ~= nil and roomClosed() == nil then
							pcall(nakama.lobbySend, 'settings', {settings = new})
						end
						guard = lobby.inputGuardFrames
						signature = nil
					elseif name == 'start' then
						pcall(nakama.lobbySend, 'start')
					elseif name == 'watch' then
						lobby.f_watchMatch()
						guard = lobby.afterMatchGuardFrames
						signature = nil
					elseif name == 'players' then
						focus = 'players'
						cursorIndex = 1
						for i, member in ipairs(members) do
							if member.user_id == nakama.userId() then
								cursorIndex = i
							end
						end
					end
				else
					menuMove(m)
				end
			end
			drawBackground()
			local shown = m
			if focus == 'playermenu' then
				shown = pm
			end
			drawRoom(shown, focus, (focus == 'players' or focus == 'playermenu') and cursorIndex or nil, entry)
			drawForeground()
			refresh()
		end
	end
end

-- ---------------------------------------------------------------------------
-- Title menu item

-- main.t_itemname.onlinelobby: returns the function the title menu runs
-- after its fade out.
function lobby.f_menu(t, item)
	if nakama == nil then
		return nil
	end
	hook.run('main.t_itemname', t, item)
	return function()
		main.f_waitForPreloads(true)
		lobby.f_browser()
		esc(false)
		resetKey()
		bgReset(motif[main.background].BGDef)
		fadeInInit(motif[main.group].fadein.FadeData)
		playBgm({source = 'motif.title', interrupt = true})
	end
end

return lobby
