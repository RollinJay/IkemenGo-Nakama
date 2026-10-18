-- CBR AI: applies the CBR settings before every fight.
--
-- Files in external/mods load automatically; renaming this file to -cbr.lua
-- (any name starting with '-') disables the mod entirely. Learned data is
-- saved per character under save/cbr.
--
-- The main settings are in Options > Game Settings > CBR AI Settings: CBR AI,
-- Knowledge, Training Dummy and Learn From Replays. All ten settings are in
-- the [CBR] section of save/config.ini:
--
--   Enabled          turns the CBR layer on or off (CBR AI in the options).
--   DriveCPU         CPU-controlled characters play with the CBR layer. Where
--                    it has nothing close enough to the situation, the
--                    engine's AI plays instead.
--   Execution        1-8, or 0 to follow each CPU's AI level (the Difficulty
--                    option). Below 8, combo timing is perturbed and long
--                    routes are valued less.
--   Knowledge        1-8: how much of what was learned the CPU may use.
--                    Execution and Knowledge both at 8 unlock resets, OTG
--                    relaunches and loops.
--   Ambition         1-8, or 0 to follow Execution: how long and damaging the
--                    routes the CPU goes for are.
--   RecordHumans     learn from people playing.
--   RecordCPU        learn from CPU play (the engine AI and character AI).
--   RecordReplays    also learn from replays: match replays played back and
--                    lobby matches watched live, each of those learned once
--                    (Learn From Replays in the options).
--   RecordInTraining also learn in training mode. Off by default: play
--                    against a dummy that does not fight back would teach
--                    that everything works.
--   DriveInTraining  also drive the training dummy when it is CPU-controlled
--                    (Training Dummy in the options).
--
-- The settings are read again before every fight (the start.f_game hook), so
-- a change applies from the next fight, without a restart. That covers fights
-- started from the menus, replays and watched lobby matches. Replays are never
-- driven, and neither is a match being saved as a replay (a local versus
-- fight, or a mode in replay.recordModes): its CPU players are the engine's
-- own AI, so that the replay reproduces the match. The CBR layer is inactive
-- during online matches. A quick match started from the command line, the
-- demo fights (title screen and attract mode) and the random test start their
-- fight directly instead of through start.f_game, so they skip this hook: they
-- keep whatever CBR state the last fight applied, which is off when no fight
-- has applied any yet.

-- Each setting: its name in cbrApply, its key in the [CBR] section, and the
-- value used if the engine has no such key.
local t_settings = {
	{'enabled', 'Enabled', true},
	{'driveCPU', 'DriveCPU', true},
	{'execution', 'Execution', 0},
	{'knowledge', 'Knowledge', 8},
	{'ambition', 'Ambition', 0},
	{'recordHumans', 'RecordHumans', true},
	{'recordCPU', 'RecordCPU', true},
	{'recordReplays', 'RecordReplays', true},
	{'recordInTraining', 'RecordInTraining', false},
	{'driveInTraining', 'DriveInTraining', false},
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

local function cbrApply()
	local cfg = cbrSettings()
	cbrEnable(cfg.enabled)
	if not cfg.enabled then
		return
	end
	local training = gameMode('training')
	local record = (cfg.recordHumans or cfg.recordCPU) and (cfg.recordInTraining or not training)
	local drive = cfg.driveCPU and (cfg.driveInTraining or not training)
	cbrRecordWho(cfg.recordHumans, cfg.recordCPU, cfg.recordReplays)
	for pn = 1, 8 do
		cbrStop(pn)
		if record then
			cbrRecord(pn)
		end
		if drive then
			cbrDrive(pn, cfg.execution, cfg.knowledge, cfg.ambition)
		end
	end
end

hook.add('start.f_game', 'cbr', cbrApply)
