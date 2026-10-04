-- CBR AI: settings, applied before every fight.
--
-- Files in external/mods load automatically; renaming this file to start
-- with '-' disables it. Learned data is saved per character under save/cbr.
--
--   enabled          turns the CBR layer on or off.
--   driveCPU         CPU-controlled characters play with the CBR layer. Where
--                    it has nothing close enough to the situation, the
--                    engine's AI plays instead.
--   execution        1-8, or 0 to follow each CPU's AI level (the Difficulty
--                    option). Below 8, combo timing is perturbed and long
--                    routes are valued less.
--   knowledge        1-8: how much of what was learned the CPU may use.
--                    Execution and knowledge both at 8 unlock resets, OTG
--                    relaunches and loops.
--   ambition         1-8, or 0 to follow execution: how long and damaging the
--                    routes the CPU goes for are.
--   recordHumans     learn from people playing.
--   recordCPU        learn from CPU play (the engine AI and character AI).
--   recordReplays    also learn from replays: match replays played back and
--                    lobby matches watched live. Each match is learned once.
--   recordInTraining also learn in training mode. Off by default: play
--                    against a dummy that does not fight back would teach
--                    that everything works.
--   driveInTraining  also drive the training dummy when it is CPU-controlled.
--
-- The settings apply before every fight (the start.f_game hook): fights
-- started from the menus, replays and watched lobby matches. Replays are
-- never driven, and neither is a match being saved as a replay (a local
-- versus fight, or a mode in replay.recordModes): its CPU players are the
-- engine's own AI, so that the replay reproduces the match. The CBR layer is
-- inactive during online matches. A quick match from the command line starts
-- before mods load and does not get these settings.
cbrSettings = {
	enabled = true,
	driveCPU = true,
	execution = 0,
	knowledge = 8,
	ambition = 0,
	recordHumans = true,
	recordCPU = true,
	recordReplays = true,
	recordInTraining = false,
	driveInTraining = false,
}

local function cbrApply()
	local cfg = cbrSettings
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
