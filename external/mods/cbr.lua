-- CBR AI: applies the CBR settings before every fight, and can ask before a
-- mode whether to fight and train the CBR AI.
-- Based on KDing's original CBR AI for IKEMEN GO (https://github.com/KDing0).
--
-- The CBR AI (case-based reasoning) learns from the fights it sees: the
-- situations, what the players did in them and how it turned out. The engine
-- side is the "CBR layer". Files in external/mods load automatically;
-- renaming this file to -cbr.lua (any name starting with '-') disables the
-- mod entirely. Learned data is saved per character under save/cbr.
--
-- How it plays. It works with any kind of character AI: a character with an
-- AI of its own keeps playing with it, and one without plays with the
-- engine's AI, as it would without the CBR AI. Whichever AI the character
-- plays with is its base AI, which plays the CPU-controlled character as
-- usual whenever the CBR AI has not taken over. The CBR AI takes over only
-- with a move that its own plays have proved better than the base AI in the
-- same kind of situation, by at least the take-over margin (TakeoverMargin
-- below). While that move plays, the base AI pauses (a character's own AI
-- sees its AILevel trigger read 0); then it carries on. Moves are compared
-- by their average result per exchange (an attack or combo and what came of
-- it): damage dealt, weighted by how well the exchange ended, minus damage
-- taken. Now and then the CBR AI tries a move it has not proved yet, to find
-- out whether it is better: at about one in twenty of the moments it could
-- try one (counting one each time the situation changes), more often for a
-- move that looks promising, and a few times a round at most. An AI that
-- keeps itself switched on in a variable (one a long command turns on, or
-- one that notes AILevel once and then only checks its variable), or that
-- never reads AILevel, cannot be paused and keeps running; when such an AI
-- starts an attack during a move the CBR AI is playing, that move is usually
-- dropped without counting against it.
--
-- Where its moves come from. The CBR AI learns moves from people playing the
-- character, and from CPU play by the engine's AI or by a character AI that
-- inputs commands. From a character AI that changes states directly (the
-- most common kind) it learns only how well that AI does, so with such an AI
-- it takes over only once people have played the character.
--
-- The main settings are in Options > Game Settings > CBR AI Settings: CBR AI,
-- Knowledge, Take-Over Margin, Training Dummy, Learn From Replays and Ask
-- Before Modes. All twelve settings are in the [CBR] section of
-- save/config.ini:
--
--   Enabled          turns the CBR layer on or off (CBR AI in the options).
--   DriveCPU         the CBR AI may take over CPU-controlled characters, as
--                    described above.
--   TakeoverMargin   how much better, per exchange, a move must have proved
--                    than the base AI before the CBR AI takes over with it:
--                    1 Low, 2 Medium or 3 High (Take-Over Margin in the
--                    options). Medium asks for a few percent of a life bar,
--                    Low half as much and High twice as much. At first a
--                    move must clear a higher bar, which comes down as it is
--                    played more (it is also higher while little is known
--                    about how the base AI does in that situation). Low lets
--                    the CBR AI take over more readily; High only with
--                    clearly better moves.
--   Execution        1-8, or 0 to follow each CPU's AI level (the Difficulty
--                    option). Below 8, combo timing is perturbed and long
--                    routes are valued less.
--   Knowledge        1-8: how much of what was learned the CBR AI may use.
--                    Execution and Knowledge both at 8 unlock resets, OTG
--                    relaunches and loops.
--   Ambition         1-8, or 0 to follow Execution: how long and damaging the
--                    routes the CBR AI goes for are.
--   RecordHumans     learn from people playing.
--   RecordCPU        learn from CPU play (the engine AI, and a character AI
--                    that inputs commands). Whatever RecordHumans and
--                    RecordCPU say, while the CBR AI may take over it keeps
--                    track of how the base AI does and how the moves it
--                    plays do, since that is what it compares; the two
--                    choose which play it learns new moves from.
--   RecordReplays    also learn from replays: match replays played back and
--                    lobby matches watched live, each of those learned once
--                    (Learn From Replays in the options).
--   RecordInTraining also learn in training mode. Off by default: play
--                    against a dummy that does not fight back would teach
--                    that everything works.
--   DriveInTraining  also let the CBR AI take over the training dummy when it
--                    is CPU-controlled (Training Dummy in the options).
--   AskBeforeMode    ask two questions before an offline mode with CPU
--                    opponents (Ask Before Modes in the options; see below).
--
-- The settings are read again before every fight (the start.f_game hook), so
-- a change applies from the next fight, without a restart. That covers fights
-- started from the menus, replays and watched lobby matches. The CBR AI never
-- takes over in a replay, nor in a match being saved as a replay (a local
-- versus fight, or a mode in replay.recordModes): its CPU players play with
-- their own AI alone, so that the replay reproduces the match. The CBR layer
-- is inactive during online matches. A quick match started from the command
-- line, the demo fights (title screen and attract mode) and the random test
-- start their fight directly instead of through start.f_game, so they skip
-- this hook: they keep whatever CBR state the last fight applied, which is
-- off when no fight has applied any yet.
--
-- The questions. With the CBR AI and AskBeforeMode on, picking an offline
-- mode where people play against the CPU (arcade, team arcade, team co-op,
-- survival, survival co-op, time attack, bonus games, quick match, story
-- arcs) first asks "Fight the CBR AI?", then "Train the CBR AI?", on the
-- menu's background and before character select. The cursor starts on Yes;
-- up and down (or left and right) move it, a confirm button answers, and the
-- menu's cancel button or Esc goes back to the main menu without starting
-- the mode. The answers hold for the whole mode, continues included, until
-- the player is back at the main menu. The first decides whether the CBR AI
-- may take over the CPU players (in place of DriveCPU; with No, they play
-- with their own AI alone), the second whether it learns new moves from the
-- fights (RecordHumans, RecordCPU and RecordReplays still choose whom it
-- learns them from). Either way, while the CBR AI may take over, it keeps
-- track of how the base AI does and how the moves it plays do, since that
-- is what it compares. How it plays stays as set in the options
-- (TakeoverMargin, Execution, Knowledge, Ambition). Nothing is asked in
-- training, watch, the versus modes, netplay, online play or replays, or
-- with AskBeforeMode off; the settings then apply as they are.

-- Each setting: its name in cbrApply, its key in the [CBR] section, and the
-- value used if the engine has no such key.
local t_settings = {
	{'enabled', 'Enabled', true},
	{'driveCPU', 'DriveCPU', true},
	{'takeoverMargin', 'TakeoverMargin', 2},
	{'execution', 'Execution', 0},
	{'knowledge', 'Knowledge', 8},
	{'ambition', 'Ambition', 0},
	{'recordHumans', 'RecordHumans', true},
	{'recordCPU', 'RecordCPU', true},
	{'recordReplays', 'RecordReplays', true},
	{'recordInTraining', 'RecordInTraining', false},
	{'driveInTraining', 'DriveInTraining', false},
	{'askBeforeMode', 'AskBeforeMode', true},
}

-- The current settings. gameOption raises an error for a key the engine does
-- not have, so each read is protected and falls back to its default.
local function cbrSettings()
	local cfg = {}
	for _, v in ipairs(t_settings) do
		local name, key, default = v[1], v[2], v[3]
		local ok, value = pcall(gameOption, 'CBR.' .. key)
		if ok and type(value) == type(default) then
			cfg[name] = value
		else
			cfg[name] = default
		end
	end
	return cfg
end

-- The answers to the questions for the mode being played, as
-- {fight = true or false, train = true or false}. nil when nothing was
-- asked (AskBeforeMode is off, or the mode does not ask), and once the
-- player is back at the main menu.
local answers = nil

local function cbrApply()
	local cfg = cbrSettings()
	cbrEnable(cfg.enabled)
	if not cfg.enabled then
		return
	end
	local training = gameMode('training')
	local record = (cfg.recordHumans or cfg.recordCPU) and (cfg.recordInTraining or not training)
	local drive = cfg.driveCPU and (cfg.driveInTraining or not training)
	-- The answers, while they hold, decide instead; whom it learns from still
	-- follows the settings (cbrRecordWho).
	if answers ~= nil then
		record = answers.train
		drive = answers.fight
	end
	cbrRecordWho(cfg.recordHumans, cfg.recordCPU, cfg.recordReplays)
	for pn = 1, 8 do
		cbrStop(pn)
		if record then
			cbrRecord(pn)
		end
		if drive then
			cbrDrive(pn, cfg.execution, cfg.knowledge, cfg.ambition, cfg.takeoverMargin)
		end
	end
end

hook.add('start.f_game', 'cbr', cbrApply)

-- Modes that never ask, whoever plays them: training has its own setting
-- (Training Dummy), watch is CPU against CPU, the versus modes are people
-- against people (challenger: a second player joining arcade mode), and the
-- demo fights, the random test and a quick match from the command line
-- start their fights without the menus.
local t_noAsk = {
	training = true,
	watch = true,
	versus = true,
	versuscoop = true,
	challenger = true,
	demo = true,
	randomtest = true,
	quickvs = true,
}

-- Whether the mode about to start asks the questions: an offline mode (not
-- netplay, online play or a replay) where one side is played by people and
-- the other by the CPU (main.cpuSide, set by the mode's menu item).
local function cbrShouldAsk(cfg)
	if not cfg.enabled or not cfg.askBeforeMode then
		return false
	end
	if connected() or main.replayActive or (replay ~= nil and replay.playing) then
		return false
	end
	if online ~= nil and online.f_sessionActive ~= nil and online.f_sessionActive() then
		return false
	end
	local mode = gameMode() or ''
	if t_noAsk[mode] or mode:match('^netplay') then
		return false
	end
	local cpu = main.cpuSide or {}
	return (cpu[1] == true) ~= (cpu[2] == true)
end

-- A [Warning Info] text.<key>.text of the screenpack, or the fallback when
-- it has none.
local function warningText(key, fallback)
	local t = motif.warning_info.text.text or {}
	local v = t[key]
	if v == nil or v == '' then
		return fallback
	end
	return v
end

-- Asks a question and returns true (Yes), false (No) or nil when the player
-- backs out. It looks like the warning screen ([Warning Info]: its overlay
-- and text) on the menu's background, with Yes and No below as items in the
-- menu's style, and uses the menu's keys and sounds. The menu state is a
-- private table, so the main menu's own cursor stays where it was.
-- first: fade in (the menu faded out before running the mode); last: fade
-- out after the answer. Backing out always fades out, back to the menu.
local function cbrAsk(question, first, last)
	local sec = motif[main.group]
	local bg = motif[main.background]
	local valuename = motif.option_info.menu.valuename
	local m = {menu = sec.menu}
	local items = {
		{itemname = 'yes', displayname = valuename.yes or 'Yes', paramname = 'yes'},
		{itemname = 'no', displayname = valuename.no or 'No', paramname = 'no'},
	}
	local item, cursorPosY, moveTxt = 1, 1, 0
	local textData = motif.warning_info.text.TextSpriteData
	textImgReset(textData)
	textImgSetText(textData, question)
	local function draw()
		clearColor(bg.bgclearcolor[1], bg.bgclearcolor[2], bg.bgclearcolor[3])
		bgDraw(bg.BGDef, 0)
		rectDraw(motif.warning_info.overlay.RectData)
		textImgDraw(textData)
		main.f_menuCommonDraw(items, item, cursorPosY, moveTxt, m, bg, true,
			{skipBG0 = true, skipBG1 = true, skipTitle = true})
		bgDraw(bg.BGDef, 1)
		refresh()
	end
	resetKey()
	esc(false)
	if first then
		fadeInInit(sec.fadein.FadeData)
	end
	local answer = nil
	while true do
		-- Each frame is drawn before the buttons are read, so the button that
		-- led here, pressed on an earlier frame, does not answer as well.
		draw()
		if fadeActive() then
			if esc() or getInput(-1, sec.menu.cancel.key) then
				esc(false)
				fadeSkip()
			end
		elseif esc() or getInput(-1, sec.menu.cancel.key) then
			esc(false)
			sndPlay(motif.Snd, sec.cancel.snd[1], sec.cancel.snd[2])
			break
		elseif getInput(-1, sec.menu.done.key) then
			local snd = sec.cursor.done.snd.default
			sndPlay(motif.Snd, snd[1], snd[2])
			answer = items[item].itemname == 'yes'
			break
		else
			cursorPosY, moveTxt, item = main.f_menuCommonCalc(items, item, cursorPosY, moveTxt, m, sec.cursor)
		end
	end
	if answer == nil or last then
		fadeOutInit(sec.fadeout.FadeData)
		while fadeActive() do
			draw()
		end
	end
	-- The button that answered does not also work the next screen.
	resetKey()
	resetTokenGuard()
	return answer
end

local function pack(...)
	return {n = select('#', ...), ...}
end

-- The questions come before start.f_selectMode, which plays a mode from
-- character select until the player is back at the main menu: each mode's
-- menu item (main.t_itemname) returns it, and the menu runs it after its
-- fade-out. The answers hold until it returns. A start.f_selectMode run
-- inside another (none of the game's own modes does that) has answers of
-- its own, and the outer run's come back after it.
local selectMode = start.f_selectMode
start.f_selectMode = function(...)
	local outer = answers
	answers = nil
	if cbrShouldAsk(cbrSettings()) then
		local fight = cbrAsk(warningText('cbrfight', 'Fight the CBR AI?'), true, false)
		local train = nil
		if fight ~= nil then
			train = cbrAsk(warningText('cbrtrain', 'Train the CBR AI?'), false, true)
		end
		if train == nil then
			-- Backed out: back to the menu without starting the mode, as
			-- start.f_selectMode does when the player leaves character
			-- select. The menu's music has kept playing.
			answers = outer
			bgReset(motif[main.background].BGDef)
			fadeInInit(motif[main.group].fadein.FadeData)
			return
		end
		answers = {fight = fight, train = train}
	end
	local ret = pack(selectMode(...))
	answers = outer
	return unpack(ret, 1, ret.n)
end
