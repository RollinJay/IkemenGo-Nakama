-- REPLAY++ MODULE by jay_ts
-- Main Lua

local util = require('external.mods.jayreplay++.util')
util.loadMotifExtensions('external/mods/jayreplay++/replay.def')

-- init things
local replay = {}

local m = motif.replay_menu
local fadeIn, fadeOut = m.fadein.FadeData, m.fadeout.FadeData
local previousFadePhase = ''
local showInputs = tostring(m.show.inputs):lower() == 'yes'
local side = 1

-- define itemnames
local sec = main.f_tableCopy(motif.pause_menu.pause_menu)
sec.title.text = m.menu.itemname.title
sec.menu.itemname_order = {'replaycontinue', 'replayshowinputs', 'replaytakeover', 'replayexit'}
sec.menu.itemname = sec.menu.itemname or {}
sec.menu.itemname.replaycontinue = m.menu.itemname.continue
sec.menu.itemname.replayshowinputs = m.menu.itemname.showinputs
sec.menu.itemname.replaytakeover = m.menu.itemname.takeover
sec.menu.itemname.replayexit = m.menu.itemname.exit

-- define valuenames
menu.t_valuename.replayshowinputs = {
	{itemname = 'no', displayname = m.menu.valuename.showinputs.no},
	{itemname = 'yes', displayname = m.menu.valuename.showinputs.yes},
}
menu.t_valuename.replaytakeover = {
	{itemname = 'p1', displayname = m.menu.valuename.takeover.p1},
	{itemname = 'p2', displayname = m.menu.valuename.takeover.p2},
}
menu.replayshowinputs = showInputs and 2 or 1
menu.replaytakeover = 1
menu.t_vardisplay.replayshowinputs = function()
	return menu.t_valuename.replayshowinputs[menu.replayshowinputs].displayname
end
menu.t_vardisplay.replaytakeover = function()
	return menu.t_valuename.replaytakeover[menu.replaytakeover].displayname
end

-- update fade info
local function updateReplayFade()
	local phase = ''
	if main.replayActive and type(replayMenuPhase) == 'function' then
		phase = replayMenuPhase()
	end
	if phase ~= previousFadePhase then
		previousFadePhase = phase
		if phase == 'openingout' or phase == 'closingout' then
			fadeOutInit(fadeOut)
		elseif phase == 'openingin' or phase == 'closingin' then
			fadeInInit(fadeIn)
		end
	end
end

-- set input map
local function setReplayMap()
	if not main.replayActive then return end
	local value = showInputs and 1 or 0
	for i = 1, 12 do
		if player(i) then
			mapSet('jayReplay.inputs', value)
		end
	end
	player(1)
end

-- use all common six-button attack bindings plus auxiliary attack keys.
local function selectionPressed()
	return getInput(1, {'a','b','c','x','y','z','s','d','w'})
		or getInput(-1, sec.menu.done.key)
end

local function closeMenu()
	menu.pauseExitDelay = gameOption('Input.PauseExitDelay')
end

-- define what options do
menu.t_itemname.replaycontinue = function(t, item, cursor, move, menuSec)
	if selectionPressed() then
		sndPlay(motif.Snd, menuSec.cursor.done.snd[1], menuSec.cursor.done.snd[2])
		closeMenu()
		return false
	end
	return true
end
menu.t_itemname.replayshowinputs = function(t, item, cursor, move, menuSec)
	if menu.f_valueChanged(t.items[item], menuSec) then
		showInputs = (menu.replayshowinputs == 2)
		setReplayMap()
	end
	return true
end
menu.t_itemname.replaytakeover = function(t, item, cursor, move, menuSec)
	menu.f_valueChanged(t.items[item], menuSec)
	side = menu.replaytakeover
	if selectionPressed() and type(replayBegin) == 'function' and replayBegin(side) then
		sndPlay(motif.Snd, menuSec.cursor.done.snd[1], menuSec.cursor.done.snd[2])
		closeMenu()
		return false
	end
	return true
end
menu.t_itemname.replayexit = function(t, item, cursor, move, menuSec)
	if selectionPressed() then
		sndPlay(motif.Snd, menuSec.cursor.done.snd[1], menuSec.cursor.done.snd[2])
		if type(replayRequestExit) == 'function' and replayRequestExit() then
			closeMenu()
		end
		return false
	end
	return true
end

-- add replay table
table.insert(menu.t_menus, {id='replay', sec=sec, bg=motif.pausebgdef.pausebgdef, movelist=false})

-- define title info
local defaultTitle = main.f_itemnameUpper(
	motif.pause_menu.pause_menu.title.text,
	motif.pause_menu.pause_menu.menu.title.uppercase)
local replayTitle = main.f_itemnameUpper(sec.title.text, sec.menu.title.uppercase)
local function primeReplayTitle()
	if sec.title and sec.title.TextSpriteData then
		textImgSetText(sec.title.TextSpriteData, replayTitle)
	end
end
local function restoreDefaultTitle()
	if sec.title and sec.title.TextSpriteData then
		textImgSetText(sec.title.TextSpriteData, defaultTitle)
	end
end

-- prime the main menu
local originalMenuRun = menu.f_run
function menu.f_run(...)
	updateReplayFade()
	if main.replayActive and menu.currentMenuId == 'replay' then
		primeReplayTitle()
	end
	return originalMenuRun(...)
end

-- restore the shared motif title when replay's native menu closes
-- we like to clean up after ourselves
local originalMenuReset = menu.f_reset
function menu.f_reset(...)
	if menu.currentMenuId == 'replay' then
		restoreDefaultTitle()
	end
	return originalMenuReset(...)
end

local origMenuInit = menu.f_init
function menu.f_init(...)
	origMenuInit(...)
	if main.replayActive and menu.replay and menu.replay.loop then
		menu.currentMenu = {menu.replay.loop, menu.replay.loop}
		menu.currentMenuId = 'replay'
		menu.replay.cursorPosY, menu.replay.item = 1, 1
		menu.replay.moveTxt = 0
		menu.replaytakeover = side
		menu.replayshowinputs = showInputs and 2 or 1
		primeReplayTitle()
	end
end

-- attach native per-frame API once the replay is entered
local replayBrowsing = false
local replayController = 1
local nativeReplayBrowser = main.f_replay
function main.f_replay(...)
	replayController = 1
	replayBrowsing = true
	local ret = nativeReplayBrowser(...)
	replayBrowsing = false
	return ret
end

-- find input display module information
local function localInputDisplayPath(groupName, filename)
	local options = loadGameOption()
	local groups = options.Common and options.Common[groupName]
	if type(groups) ~= 'table' then return nil end
	for _, paths in pairs(groups) do
		if type(paths) == 'table' then
			for _, path in ipairs(paths) do
				if type(path) == 'string' and
					path:gsub('\\', '/'):lower():match('[^/]+$') == filename then
					return path
				end
			end
		end
	end
	return nil
end

-- enter netplay protocol
-- find module, configure replay manager, set replay maps, begin
local enterReplayOriginal = enterReplay
function enterReplay(...)
	local inputDisplay = localInputDisplayPath('States', 'jayinputdisplay.zss')
	local inputFx = localInputDisplayPath('Fx', 'jayinputdisplay.def')
	local ok = enterReplayOriginal(...)
	if ok and inputDisplay and not inputFx then
		print('Replay++: jayinputdisplay.def is missing from Common.Fx; skipping display state to prevent an invalid JBTN animation.')
	end
	if ok and inputDisplay and inputFx then
		if type(replayIncludeCommonState) == 'function' then
			if not replayIncludeCommonState(inputDisplay, inputFx) then
				print('Replay++: unable to enable input display common state and FX for this replay.')
			end
		else
			print('Replay++: replayIncludeCommonState is unavailable. Rebuild the engine from source.')
		end
	end
	if ok and type(replayInstall) ~= 'function' then
		print('Replay++: native replay.go API missing. Rebuild Ikemen GO from source to activate controls.')
	end
	if ok and type(replayInstall) == 'function' then
		replayInstall()
		replayConfigure(math.floor(m.takeover.seconds*60), m.history.interval,
			m.history.count, m.rewind.speed,
			m.menu.key, m.pause.key,
			m.rewind.key, m.rewind['repeat'].ticks,
			m.fastforward.key, m.fastforward.speed,
			m.fastforward['repeat'].ticks, m.hold.ticks, m.fastforward.rate)
		if type(replaySetController) == 'function' then
			replaySetController(replayController)
		end
		setReplayMap()
	end
	return ok
end

local exitReplayOriginal = exitReplay
function exitReplay(...)
	previousFadePhase = ''
	if type(replayClean) == 'function' then replayClean() end
	return exitReplayOriginal(...)
end

-- called once per match frame by the common lua callback
function replay.tick()
	if type(replayTick) ~= 'function' then return end
	if replayTick() then
		fadeSkip()
		replayFinishExit()
		start.exit = true
		main.pauseMenuActive = false
		previousFadePhase = ''
		return
	end
	updateReplayFade()
	setReplayMap()
end

function replayPumpTick()
	replay.tick()
	return true
end

-- override replay inputs in order to navigate the replay menu
local nativeGetInput = getInput
function getInput(players, ...)
	if replayBrowsing and not main.replayActive and players == -1 then
		local pressed = nativeGetInput(players, ...)
		if pressed then
			local controller = getLastInputController()
			if controller == 1 or controller == 2 then
				replayController = controller
			end
		end
		return pressed
	end
	if main.replayActive and main.pauseMenuActive and
	   menu.currentMenuId == 'replay' and
	   type(replayMenuInput) == 'function' then
		return replayMenuInput(players, ...)
	end
	return nativeGetInput(players, ...)
end

return replay
