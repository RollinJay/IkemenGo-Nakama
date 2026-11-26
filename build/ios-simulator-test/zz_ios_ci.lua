-- iOS Simulator test mod (CI only). run.sh copies it into the test game
-- folder's external/mods; it is never part of the app or a release.
--
-- It walks the game through the screens the test screenshots, as a player
-- would, by pressing the screenpack's own menu keys (no touches needed):
--   title       the title menu is on screen
--   textbox     a text box is open (main.f_drawInput, as JOIN's name box),
--               which opens the on-screen keyboard
--   touchbuttons  Options > Input Settings > On-screen Buttons is selected
--   layout-<name> On-screen Layout set to each value in turn (R, the menu's
--               add key): default (the screenpack's), straight, vewlix,
--               arch, staggered, grid, classic; the app has the new layout
--               once the game has sent it (within 30 frames)
--   editor      back on default, Edit On-screen Buttons is confirmed, which
--               opens the app's layout editor over the buttons
-- At each step it writes "STEP <name>" to ci/steps.log in the game folder,
-- then waits until run.sh creates ci/ack-<name> (after its screenshot), or
-- for stepTimeout frames. Problems are written as "FAIL <what>", and "DONE"
-- ends the run. Each line also goes to the game's stdout as [ios-ci].

local ci = {
	frame = 0,       -- frames drawn since the mod loaded (refresh calls)
	step = 'title',  -- current step (see steps below)
	stepFrame = nil, -- frame the current step's wait began
	nextAction = 0,  -- no key press before this frame (menus animate)
	press = nil,     -- menu key token pressed this frame
	pressAt = -1,
	moves = 0,       -- cursor moves in the current menu, to give up on loops
	textbox = false, -- inside the test's text box
	layout = 0,      -- index in layouts of the On-screen Layout value shown (0: Default)
	done = false,
}
local stepTimeout = 60 * 60 -- frames to wait for an ack (60 s at 60 fps)
-- On-screen Layout's values after Default, in the menu's order
-- (options.t_touchLayouts), and the frames to wait after choosing one: the
-- game tells the app every 30 frames at most (touchControlsUpdate).
local layouts = {'straight', 'vewlix', 'arch', 'staggered', 'grid', 'classic'}
local layoutWait = 60

local function log(msg)
	print('[ios-ci] ' .. msg)
	local f = io.open('ci/steps.log', 'a')
	if f then
		f:write(msg .. '\n')
		f:close()
	end
end

local function exists(path)
	local f = io.open(path, 'r')
	if f then
		f:close()
		return true
	end
	return false
end

-- First token of a screenpack key list ("D, R" or {'D', 'R'}).
local function firstKey(keys)
	if type(keys) == 'table' then
		return keys[1]
	end
	if type(keys) == 'string' then
		return keys:match('^%s*([^,%s]+)')
	end
	return nil
end

local function hasKey(keys, k)
	if type(keys) == 'table' then
		for _, v in ipairs(keys) do
			if v == k then
				return true
			end
		end
		return false
	end
	if type(keys) == 'string' then
		for v in keys:gmatch('[^,%s]+') do
			if v == k then
				return true
			end
		end
	end
	return false
end

-- Starts waiting at a step: logs it once; true once run.sh acked it (or the
-- wait timed out).
local function stepReached(name)
	if ci.stepFrame == nil then
		ci.stepFrame = ci.frame
		log('STEP ' .. name)
	end
	if exists('ci/ack-' .. name) then
		return true
	end
	if ci.frame - ci.stepFrame > stepTimeout then
		log('FAIL no ack for ' .. name)
		return true
	end
	return false
end

local function nextStep(name)
	ci.step = name
	ci.stepFrame = nil
	ci.moves = 0
	ci.nextAction = ci.frame + 20
end

local function pressKey(token)
	if token == nil then
		return
	end
	ci.press = token
	ci.pressAt = ci.frame
	ci.nextAction = ci.frame + 15
end

-- In menu t (item selected), moves to the item called name (or ending in
-- ".name") and confirms it when confirm is true. Returns true once it is
-- selected (and confirmed).
local function goToItem(t, item, sec, name, confirm)
	if ci.frame < ci.nextAction or fadeActive() then
		return false
	end
	local target = nil
	for i, v in ipairs(t) do
		local n = v.itemname or ''
		if n == name or n:sub(-(#name + 1)) == '.' .. name then
			target = i
			break
		end
	end
	if target == nil then
		return false
	end
	local m = sec.menu or sec
	if item ~= target then
		if ci.moves > #t * 2 then
			log('FAIL could not reach menu item ' .. name)
			ci.done = true
			return false
		end
		ci.moves = ci.moves + 1
		pressKey(firstKey(m.next.key))
		return false
	end
	if confirm then
		pressKey(firstKey(m.done.key))
	end
	return true
end

-- The menus ask for keys through getInput; a pressed token counts as any
-- player's, once, in the frame it is pressed. Once only: a submenu opened by
-- a press starts its loop in the same frame, and would take it again.
local realGetInput = getInput
getInput = function(pn, ...)
	if ci.press ~= nil and ci.pressAt == ci.frame then
		for _, keys in ipairs({...}) do
			if hasKey(keys, ci.press) then
				ci.press = nil
				return true
			end
		end
	end
	return realGetInput(pn, ...)
end

-- A test press is player 1's (choosing a menu item remaps the last controller).
local realLast = getLastInputController
if realLast ~= nil then
	getLastInputController = function(...)
		local c = realLast(...)
		if c == nil or c < 1 then
			return 1
		end
		return c
	end
end

local realRefresh = refresh
refresh = function(...)
	ci.frame = ci.frame + 1
	-- Leaves the test's text box once run.sh has its screenshot.
	if ci.textbox and stepReached('textbox') then
		esc(true)
	end
	return realRefresh(...)
end

-- One frame of the test, from the menus' draw call (t: items, item: the
-- selected one, sec: the menu's motif section).
local function drive(t, item, sec)
	if ci.step == 'title' and sec == motif.title_info and not fadeActive() then
		if stepReached('title') then
			nextStep('textbox')
		end
	elseif ci.step == 'textbox' and sec == motif.title_info and ci.frame >= ci.nextAction then
		-- JOIN's name box; the on-screen keyboard opens while Lua reads typed
		-- text (src/text_entry.go).
		local ti = motif.title_info.textinput
		ci.textbox = true
		local ok, err = pcall(main.f_drawInput,
			ti.TextSpriteData,
			(ti.text and ti.text.name) or 'iOS Simulator test',
			motif.title_info,
			motif[main.background],
			ti.overlay.RectData
		)
		ci.textbox = false
		esc(false)
		resetKey()
		if not ok then
			log('FAIL text box: ' .. tostring(err))
		end
		nextStep('options')
		ci.nextAction = ci.frame + 60
	elseif ci.step == 'options' and sec == motif.title_info then
		if goToItem(t, item, sec, 'options', true) then
			nextStep('menuinput')
		end
	elseif ci.step == 'menuinput' and sec == motif.option_info then
		if goToItem(t, item, sec, 'menuinput', true) then
			nextStep('touchbuttons')
		end
	elseif ci.step == 'touchbuttons' and sec == motif.option_info then
		if goToItem(t, item, sec, 'touchbuttons', false) and stepReached('touchbuttons') then
			nextStep('touchlayout')
		end
	elseif ci.step == 'touchlayout' and sec == motif.option_info then
		if goToItem(t, item, sec, 'touchlayout', false) then
			ci.layout = 0
			nextStep('layouts')
			ci.nextAction = ci.frame + layoutWait
		end
	elseif ci.step == 'layouts' and sec == motif.option_info and ci.frame >= ci.nextAction then
		local name = layouts[ci.layout] or 'default'
		if ci.stepFrame == nil then
			local want = ci.layout == 0 and '' or name
			local got = (gameOption('Input.TouchLayout') or ''):lower()
			if got ~= want then
				log('FAIL On-screen Layout is "' .. got .. '", want "' .. want .. '"')
			end
		end
		if stepReached('layout-' .. name) then
			local m = sec.menu or sec
			-- The next value; after the last, Default again.
			pressKey(firstKey(m.add.key))
			if ci.layout < #layouts then
				ci.layout = ci.layout + 1
				ci.stepFrame = nil
				ci.nextAction = ci.frame + layoutWait
			else
				nextStep('touchedit')
			end
		end
	elseif ci.step == 'touchedit' and sec == motif.option_info then
		if goToItem(t, item, sec, 'touchedit', true) then
			nextStep('editor')
			ci.nextAction = ci.frame + layoutWait
		end
	elseif ci.step == 'editor' and ci.frame >= ci.nextAction then
		if stepReached('editor') then
			log('DONE')
			ci.done = true
		end
	end
end

local realDraw = main.f_menuCommonDraw
main.f_menuCommonDraw = function(t, item, cursorPosY, moveTxt, sec, ...)
	if not ci.done then
		-- A mistake here ends the test, not the game.
		local ok, err = pcall(drive, t, item, sec)
		if not ok then
			log('FAIL ' .. tostring(err))
			ci.done = true
		end
	end
	return realDraw(t, item, cursorPosY, moveTxt, sec, ...)
end

log('LOADED runtime=' .. tostring(getRuntimeOS and getRuntimeOS() or '?'))
