--IKEMEN GO Rematch 1.6
--This mod adds rematch support to any fight unless specified otherwise
--by turning on the ik_rematch.override value
--All values otherwise depend on the rematch.def values under [Rematch]

local function createfont(t)

local s = textImgNew()

if t.font ==nil then
t.font = motif.Fnt[2]
end

textImgSetFont(s,t.font)
if t.localcoord==nil then
t.localcoord={ t.window[3],t.window[4]}
end

textImgSetFont(s, motif.Fnt[1])
textImgSetLocalcoord(s,  t.localcoord[1] , t.localcoord[2])



--textImgSetColor(s,  255, t.g or 255, t.b or 255, t.a or 256)
textImgSetText(s, "test")
textImgSetPos(s, t.x or 100, t.y or 100)
textImgSetScale(s, t.scaleX or 1, t.scaleY or 1)
textImgSetFocalLength(s,t.flength or 2048)


--textImgDebug(s, 'new font')
return s
end

local function updatefont(s,t)
textImgReset(s)

-- Other functions are optional - use them only if you want to change the default values.
if t.text ~=nil then
textImgSetText(s, t.text)
end



if t.font ~=nil then

local fnt = motif.Fnt[t.font]
textImgSetFont(s,fnt)
end



textImgSetAccel(s, t.ax or 0, t.ay or 0)
textImgSetAlign(s, t.align or 0)
textImgSetBank(s, t.bank or 0)
textImgSetColor(s, t.r or 256, t.g or 256, t.b or 256, t.a or 256)
textImgSetPos(s, t.x or 0, t.y or 0)
textImgAddPos(s,t.sx or 0,t.sy or 0)
textImgSetFriction(s, t.fx or 0, t.fy or 0)
textImgSetLayerno(s, t.layer or 0)
if t.xDist then
textImgSetMaxDist(s, t.xDist , t.yDist)
end
textImgSetScale(s, t.scaleX or 1, t.scaleY or 1)

textImgSetTextDelay(s, t.delay or 0)
textImgSetTextSpacing(s, t.spx or 0,t.spy or 0 )
textImgSetTextWrap(s, t.wrap or 0)
textImgSetVelocity(s, t.vx or 0, t.vy or 0)
if t.source then
textImgApplyVel(s, t.source or 0)
end

textImgSetXShear(s, t.xshear or 0)
textImgSetAngle(s, t.angle or 0)
textImgSetXAngle(s, t.xangle or 0)
textImgSetYAngle(s, t.yangle or 0)
--textImgSetProjection(s, t.projection)
--textImgSetFocalLegth(s, t.length or 1)

end

--It will trigger when the starttime reaches 0, also based on your screenpack settings.
function table.contains(tbl, val)
    for _, v in ipairs(tbl) do
        if v == val then return true end
    end
    return false
end

-- usage
local tt = gameOption("Common.States")
if not table.contains(tt, "external/mods/ik_rematch.zss") then
print('not found')
    table.insert(tt, "external/mods/ik_rematch.zss")
    modifyGameOption("Common.States", tt)
end

ik_rematch = {}
ik_rematch.sd=loadIni('external/mods/rematch.def')

--ik_rematch.sd.rematchbgdef.BGDef = bgNew('data/xjl/system.sff', 'external/mods/rematch.def', 'rematch', nil, 1)

local starttime =0
if ik_rematch.sd.rematch ~= nil then

ik_rematch.main = {TextSpriteData=createfont(
	{
	fontTuple=ik_rematch.sd.rematch.font,
	window={0,0,motif.info.localcoord[1],motif.info.localcoord[2]}
	})}
	
starttime=  ik_rematch.sd.rematch.starttime
end
ik_rematch.p = {}
ik_rematch.pa={}
ik_rematch.override=false
ik_rematch.wincount={0,0}



local hv =0

local txt_yes = {}
for i = 1, 2 do
	table.insert(txt_yes,{TextSpriteData=createfont(
	{
	window={0,0,motif.info.localcoord[1],motif.info.localcoord[2]}
	})})

	 --main.f_createTextImg(motif.select_info, 'selected_p'..i)
end

local txt_no =  {}
for i = 1, 2 do
	table.insert(txt_no,{TextSpriteData=createfont(
	{
	window={0,0,motif.info.localcoord[1],motif.info.localcoord[2]}
	})})

	 --main.f_createTextImg(motif.select_info, 'selected_p'..i)
end

-- Online rematch UI is deliberately separate from the legacy two-choice UI so
-- existing offline modes keep Kamekaze's original behavior.
local onlineText = {{}, {}}
for side = 1, 2 do
	for choice = 1, 3 do
		onlineText[side][choice] = {TextSpriteData = createfont({
			window = {0, 0, motif.info.localcoord[1], motif.info.localcoord[2]}
		})}
	end
end
local onlineWaitingText = {TextSpriteData = createfont({
	window = {0, 0, motif.info.localcoord[1], motif.info.localcoord[2]}
})}

local online = {
	active = false,
	prompt = false,
	choice = {1, 1},
	sent = {false, false},
	navHeld = {false, false},
	decision = nil,	
	lastRound = 1,
	matchId = '',
	applied = false,
	overrideCaptured = false,
	previousOverride = false,
}

ik_rematch.cursor={true,true}
ik_rematch.done={false,false}
local fade =false
local winadd=false

function ik_rematch.rematchend()

return ik_rematch.done[1] and ik_rematch.done[2]
end

local function rematchmode(p)
local result = false

if ik_rematch.sd.rematch.enabledmodes==nil then
return true
end

 for _, v in pairs(ik_rematch.sd.rematch.enabledmodes) do
        if v == gameMode() then 
            result = true 
        end
    end


return result
end

local function rematchtxtpos(p)

if p== nil then
if ik_rematch.sd.rematch[gameMode()] == nil then
return ik_rematch.sd.rematch.rematch.offset
end

 return ik_rematch.sd.rematch[gameMode()].rematch.offset
else

if ik_rematch.sd.rematch[gameMode()] == nil then

return {ik_rematch.sd.rematch['p'..p].offset,ik_rematch.sd.rematch['p'..p].spacing}
end

 return {ik_rematch.sd.rematch[gameMode()]['p'..p].offset,ik_rematch.sd.rematch[gameMode()]['p'..p].spacing}
 end
end

local onlineRankedSetEndsOnCurrentMatch

local function isOnlineRematchMode()
	return nakama ~= nil and nakama.currentMatch ~= nil and nakama.currentMatch() ~= '' and gameMode('netplayversus')
end

local function onlineConfig(side)
	local cfg = ik_rematch.sd.rematchonline
	if cfg == nil then return nil end
	return cfg['p' .. side] or cfg.p1
end

local function onlineRankedState()
	if nakama == nil or nakama.rankedSet == nil then return nil end
	local ok, rs = pcall(nakama.rankedSet)
	if not ok or type(rs) ~= 'table' then return nil end
	return rs
end

local function onlineRankedActive()
	local rs = onlineRankedState()
	return rs ~= nil and rs.active == true
end

local function onlineChoiceCount()
	local rs = onlineRankedState()
	if rs ~= nil and rs.active and onlineRankedSetEndsOnCurrentMatch() then
		return 2
	end
	return 3
end

local function onlineChoiceAction(side, choice)
	local rs = onlineRankedState()
	local finalSet = rs ~= nil and rs.active and onlineRankedSetEndsOnCurrentMatch()
	if finalSet then
		if choice == 1 then return 'new_set' end
		return 'exit'
	end
	if choice == 1 then return 'rematch' end
	if choice == 2 then return 'select' end
	if rs ~= nil and rs.active then return 'forfeit' end
	return 'exit'
end

local function onlineChoiceText(side, choice)
	local cfg = onlineConfig(side)
	if cfg == nil then return '' end
	local action = onlineChoiceAction(side, choice)
	if action == 'new_set' then return (cfg.newset and cfg.newset.text) or 'Play Another Set' end
	if action == 'rematch' then return cfg.rematch.text end
	if action == 'select' then return cfg.select.text end
	if action == 'forfeit' then return (cfg.forfeit and cfg.forfeit.text) or 'Forfeit' end
	return cfg.exit.text
end

local function onlinePromptText()
	local rs = onlineRankedState()
	if rs ~= nil and rs.active and onlineRankedSetEndsOnCurrentMatch() then
		local prompt = ik_rematch.sd.rematchonline.newset
		return (prompt and prompt.prompt and prompt.prompt.text) or 'Play Another Set?'
	end
	return ik_rematch.sd.rematch.rematch.text
end

local function onlineCaptureSnapshot()
	if start.f_captureOnlineRematchSnapshot ~= nil then
		return start.f_captureOnlineRematchSnapshot()
	end
	return start.onlineLastFightSnapshot
end

onlineRankedSetEndsOnCurrentMatch = function()
	if nakama == nil or nakama.rankedSet == nil then return false end
	local rs = nakama.rankedSet()
	if type(rs) ~= 'table' or not rs.active then return false end
	local winnerSide = tonumber(getWinnerTeam()) or 0
	if winnerSide < 1 or winnerSide > 2 then return false end
	local player = (tonumber(rs.p1_side) == winnerSide) and 1 or ((tonumber(rs.p2_side) == winnerSide) and 2 or 0)
	if player < 1 then return false end
	local wins = player == 1 and tonumber(rs.p1_wins or 0) or tonumber(rs.p2_wins or 0)
	local bestOf = tonumber(rs.best_of or 3) or 3
	local needed = math.floor(bestOf / 2) + 1
	return wins + 1 >= needed
end

local function onlineApplyDecision()
	if online.decision == nil or online.applied then return end
	local d = online.decision
	local action = d.action or ''
	if action ~= 'rematch' and action ~= 'select' and action ~= 'exit' and action ~= 'forfeit' and action ~= 'new_set' then
		return
	end
	online.applied = true
	online.prompt = false
	if action == 'rematch' then
		start.onlineRematchSnapshot = d.snapshot
		start.onlineNextAction = 'rematch'
	elseif action == 'select' then
		start.onlineNextAction = 'select'
	elseif action == 'forfeit' then
		start.onlineForfeitUserId = d.source_user_id or ''
		start.onlineNextAction = 'forfeit'
	elseif action == 'new_set' then
		start.onlineNextAction = 'new_set'
	elseif action == 'exit' then
		start.onlineNextAction = 'exit'
	end
end

local function onlineSubmit(side)
	if online.sent[side] then return end
	local action = onlineChoiceAction(side, online.choice[side])
	local snapshot = nil
	if action == 'rematch' then
		snapshot = onlineCaptureSnapshot()
		if snapshot == nil then
			print('Nakama rematch: no VS snapshot available; using Character Select instead')
			action = 'select'
		end
	end
	local setFinal = onlineRankedActive() and onlineRankedSetEndsOnCurrentMatch() or false
	local ok, err = pcall(nakama.rematchChoice, action, snapshot, online.lastRound, setFinal)
	if not ok then
		print('Nakama rematch choice failed: ' .. tostring(err))
		return
	end
	online.sent[side] = true
	if not online.prompt then online.prompt = true end
end

function ik_rematch.nakamaDecision(_, event)
	local payload = event and event.payload or nil
	if type(payload) ~= 'table' then return end
	if payload.kind ~= 'ikemen-rematch-decision' or tonumber(payload.version or 0) ~= 1 then return end
	local round = tonumber(payload.round or online.lastRound) or online.lastRound
	if round < online.lastRound then return end
	online.lastRound = round + 1
	online.decision = {action = payload.action, snapshot = payload.snapshot, source_user_id = payload.source_user_id or ''}
	online.applied = false
end

if nakama ~= nil and nakama.on ~= nil then
	nakama.on('rematch_decision', ik_rematch.nakamaDecision)
end

function ik_rematch.runOnline()
	if ik_rematch.sd.rematchonline == nil then return end
	if not isOnlineRematchMode() then
		if online.active and online.overrideCaptured then
			ik_rematch.override = online.previousOverride
			online.overrideCaptured = false
		end
		online.active = false
		return false
	end
	local matchId = nakama.currentMatch()
	if online.active and online.matchId ~= matchId then
		-- A new coordination match restarts its decision rounds at 1.
		online.active = false
	end
	if not online.active then
		online.active = true
		online.matchId = matchId
		online.prompt = false
		online.sent = {false, false}
		online.choice = {1, 1}
		online.navHeld = {false, false}
		online.decision = nil
		online.lastRound = 1
		online.applied = false
		if not online.overrideCaptured then
			online.overrideCaptured = true
			online.previousOverride = ik_rematch.override
			-- Disable Kamekaze's local Yes/No flow while this Nakama session is active.
			ik_rematch.override = true
		end
	end

	-- Lua state is not part of the rollback snapshot: running this during GGPO
	-- re-simulation would replay inputs/sends against stale menu state.
	if nakama.inRollback ~= nil and nakama.inRollback() then
		return true
	end

	if roundStart() then
		online.prompt = false
		online.sent = {false, false}
		online.choice = {1, 1}
		online.navHeld = {false, false}
		online.decision = nil
		online.applied = false
	end

	if online.decision ~= nil then
		onlineApplyDecision()
		if online.applied then
			for i = 1, 2 do
				player(i)
				mapSet('ik_rematch_on', 0, 'set')
			end
			togglePause(false)
			main.pauseMenu = false
			closeMenu()
			endMatch()
			return true
		end
	end

	if matchOver() and not online.prompt and not online.sent[1] and not online.sent[2] then
		online.prompt = true
		for i = 1, 2 do player(i); mapSet('ik_rematch_on', 1, 'set') end
	end
	if not online.prompt then return true end

	updatefont(ik_rematch.main.TextSpriteData, {
		font = ik_rematch.sd.rematch.rematch.font[1],
		bank = ik_rematch.sd.rematch.rematch.font[2],
		align = ik_rematch.sd.rematch.rematch.font[3],
		text = onlinePromptText(),
		x = rematchtxtpos()[1],
		y = rematchtxtpos()[2],
		scaleX = ik_rematch.sd.rematch.rematch.scale[1],
		scaleY = ik_rematch.sd.rematch.rematch.scale[2],
		r = ik_rematch.sd.rematch.rematch.font[4],
		g = ik_rematch.sd.rematch.rematch.font[5],
		b = ik_rematch.sd.rematch.rematch.font[6],
		layer = 2,
	})
	textImgDraw(ik_rematch.main.TextSpriteData)

	-- Both peers see both players' synchronized inputs; each machine submits
	-- only its own player's choice, otherwise one press counts as both players.
	local localSide = (nakama.localSide ~= nil) and nakama.localSide() or 0
	for side = 1, 2 do
		player(side)
		if side ~= localSide then
			-- The opponent's menu is theirs to drive; nothing to read here.
		elseif not online.sent[side] and aiLevel() == 0 then
			if getInput({side}, ik_rematch.sd.rematch.accept.key) then
				sndPlay(motif.Snd, ik_rematch.sd.rematch['p'..side].done.snd[1], ik_rematch.sd.rematch['p'..side].done.snd[2])
				onlineSubmit(side)
			end
			local nav = commandGetState(side, 'U') or commandGetState(side, 'D')
			if nav and not online.navHeld[side] then
				local dir = commandGetState(side, 'U') and -1 or 1
				local count = onlineChoiceCount()
				online.choice[side] = online.choice[side] + dir
				if online.choice[side] < 1 then online.choice[side] = count end
				if online.choice[side] > count then online.choice[side] = 1 end
				sndPlay(motif.Snd, ik_rematch.sd.rematch['p'..side].cursor.snd[1], ik_rematch.sd.rematch['p'..side].cursor.snd[2])
			end
			online.navHeld[side] = nav
		end

		local cfg = side == localSide and onlineConfig(side) or nil
		if cfg ~= nil then
			for choice = 1, onlineChoiceCount() do
				local active = online.choice[side] == choice and not online.sent[side]
				local font = active and cfg.active.font or cfg.font
				updatefont(onlineText[side][choice].TextSpriteData, {
					font = font[1],
					bank = font[2],
					align = font[3],
					text = onlineChoiceText(side, choice),
					x = cfg.offset[1],
					y = cfg.offset[2] + (choice - 1) * cfg.spacing[2],
					scaleX = cfg.scale[1],
					scaleY = cfg.scale[2],
					r = font[4],
					g = font[5],
					b = font[6],
					layer = 2,
				})
				textImgDraw(onlineText[side][choice].TextSpriteData)
			end
		end

		if online.sent[side] and ik_rematch.sd.rematchonline.waiting ~= nil then
			updatefont(onlineWaitingText.TextSpriteData, {
				font = ik_rematch.sd.rematchonline.waiting.font[1],
				bank = ik_rematch.sd.rematchonline.waiting.font[2],
				align = ik_rematch.sd.rematchonline.waiting.font[3],
				text = ik_rematch.sd.rematchonline.waiting.text,
				x = ik_rematch.sd.rematchonline.waiting.offset[1],
				y = ik_rematch.sd.rematchonline.waiting.offset[2],
				scaleX = ik_rematch.sd.rematchonline.waiting.scale[1],
				scaleY = ik_rematch.sd.rematchonline.waiting.scale[2],
				r = ik_rematch.sd.rematchonline.waiting.font[4],
				g = ik_rematch.sd.rematchonline.waiting.font[5],
				b = ik_rematch.sd.rematchonline.waiting.font[6],
				layer = 2,
			})
			textImgDraw(onlineWaitingText.TextSpriteData)
		end
	end
	return true
end

local enabled =true

function ik_rematch.run()

if ik_rematch.sd.rematch ==nil then
return 
end

if isOnlineRematchMode() then
	ik_rematch.runOnline()
	return
end

if rematchmode() then
for i =1,2 do
player(i)
mapSet('ikr_wincount',ik_rematch.wincount[i],'set')
end

end



if not ik_rematch.override and roundStart() then

enabled=rematchmode()
starttime = ik_rematch.sd.rematch.starttime or 1

if start.t_victory~=nil then
start.t_victory.active=false
end
start.victoryInit=false
ik_rematch.cursor={true,true}
ik_rematch.done={false,false}
fade=false
for i =1,2 do
player(i)
mapSet('ik_rematch',0,'set')
mapSet('ik_rematch_on',0,'set')
end
winadd=false
hv =0
end

local canRematch = (((player(1) and aiLevel()==0 and lose()) or (player(2) and aiLevel()==0 and lose()))  or (gameMode('watch') or gameMode('freebattle'))) and enabled

if  matchOver() and not ik_rematch.rematchend() then
for i =1,2 do
player(i)
mapSet('ik_rematch_on',1,'set')
end

	   if not winadd then
		if player(1) and win() then

	   ik_rematch.wincount[1] = ik_rematch.wincount[1]+1
	   else
	    ik_rematch.wincount[2] = ik_rematch.wincount[2]+1
	   end
	   winadd=true
	   end
end

if not ik_rematch.override and ik_rematch.sd.rematch.victoryscreen==0 then




if  ik_rematch.sd.rematch.enabled>=1 and matchOver() and (roundState()==4 )  and (canRematch or map('ik_rematch')>0) then 
if starttime>0 then
starttime=starttime-1

else

if not ik_rematch.rematchend() then
for i =1,2 do
player(i)
mapSet('ik_rematch',1,'set')
end

end
end



if (player(1) and  map('ik_rematch') >0 and starttime==0 ) and (not ik_rematch.rematchend()) then
if not fade and (ik_rematch.sd.rematch.pausegame>=1 ) then
togglePause(true)
fade=true
end

--bgDraw(ik_rematch.sd.rematchbgdef.BGDef, 0)


		updatefont(ik_rematch.main.TextSpriteData,{
							font =   ik_rematch.sd.rematch.rematch.font[1],
							bank =   ik_rematch.sd.rematch.rematch.font[2],
							align =   ik_rematch.sd.rematch.rematch.font[3],
							text =  ik_rematch.sd.rematch.rematch.text,
							x =      rematchtxtpos()[1],
							y =      rematchtxtpos()[2],
							scaleX = ik_rematch.sd.rematch.rematch.scale[1],
							scaleY = ik_rematch.sd.rematch.rematch.scale[2],
							r =      ik_rematch.sd.rematch.rematch.font[4],
							g =      ik_rematch.sd.rematch.rematch.font[5],
							b =      ik_rematch.sd.rematch.rematch.font[6],
							layer= 2
						})

	textImgDraw(ik_rematch.main.TextSpriteData)
	for side =1,2 do
	player(side) 
		if (aiLevel()==0 or gameMode('watch')) then
			if not ik_rematch.done[side] then
					
				if getInput({side}, ik_rematch.sd.rematch.accept.key) then
					sndPlay(motif.Snd, ik_rematch.sd.rematch['p'..side].done.snd[1], ik_rematch.sd.rematch['p'..side].done.snd[2])
					ik_rematch.done[side]=true
				end
			
				if (commandGetState(side, 'U')  or commandGetState(side, 'D'))  then
				
					sndPlay(motif.Snd, ik_rematch.sd.rematch['p'..side].cursor.snd[1], ik_rematch.sd.rematch['p'..side].cursor.snd[2])
					if ik_rematch.cursor[side] then
						ik_rematch.cursor[side]=false
					else
						ik_rematch.cursor[side]=true
					end
				end
					
		end

		if ik_rematch.cursor[side] then
			updatefont(txt_yes[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].active.font[1],
				bank =   ik_rematch.sd.rematch['p'..side].active.font[2],
				align =  ik_rematch.sd.rematch['p'..side].active.font[3],
				text =  ik_rematch.sd.rematch['p'..side].yes.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].active.font[4],
				g =      ik_rematch.sd.rematch['p'..side].active.font[5],
				b =      ik_rematch.sd.rematch['p'..side].active.font[6],
				layer= 2
				})
				
				updatefont(txt_no[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].font[1],
				bank =   ik_rematch.sd.rematch['p'..side].font[2],
				align =  ik_rematch.sd.rematch['p'..side].font[3],
				text =  ik_rematch.sd.rematch['p'..side].no.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				sx=rematchtxtpos(side)[2][1],
				sy=rematchtxtpos(side)[2][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].font[4],
				g =      ik_rematch.sd.rematch['p'..side].font[5],
				b =      ik_rematch.sd.rematch['p'..side].font[6],
				layer= 2
				})
			else
				updatefont(txt_yes[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].font[1],
				bank =   ik_rematch.sd.rematch['p'..side].font[2],
				align =  ik_rematch.sd.rematch['p'..side].font[3],
				text =  ik_rematch.sd.rematch['p'..side].yes.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].font[4],
				g =      ik_rematch.sd.rematch['p'..side].font[5],
				b =      ik_rematch.sd.rematch['p'..side].font[6],
				layer= 2
				})
				
				updatefont(txt_no[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].active.font[1],
				bank =   ik_rematch.sd.rematch['p'..side].active.font[2],
				align =  ik_rematch.sd.rematch['p'..side].active.font[3],
				text =  ik_rematch.sd.rematch['p'..side].no.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				sx=rematchtxtpos(side)[2][1],
				sy=rematchtxtpos(side)[2][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].active.font[4],
				g =      ik_rematch.sd.rematch['p'..side].active.font[5],
				b =      ik_rematch.sd.rematch['p'..side].active.font[6],
				layer= 2
				})
			end
			textImgDraw(txt_yes[side].TextSpriteData)
			textImgDraw(txt_no[side].TextSpriteData)
					else
					    ik_rematch.done[side] =true
					end
					
		end
		if ik_rematch.rematchend() then
		togglePause(false)
		main.pauseMenu = false
		closeMenu()
		if ik_rematch.cursor[1] and ik_rematch.cursor[2] then
		
		reload()
		else
		for i =1,2 do
		player(i)
		mapSet('ik_rematch_on',0,'set')
		endMatch()
start.characterchange = true
		end

		end
		end
end

end
end
end

 
 
 local victoryend=false
function ik_rematch.victory() 

if ik_rematch.sd.rematch ==nil then
return 
end

if isOnlineRematchMode() then
	-- The online path is driven from loop/main-thread events. The loop hook keeps the
	-- decision visible regardless of whether the motif uses a victory screen.
	return
end


if start.t_victory ~=nil then
victoryend = start.t_victory.textend
end
 
if ik_rematch.sd.rematch.victoryscreen==0 or ik_rematch.sd.rematch.endabled==0 then
ik_rematch.done={true,true}
end

if not ik_rematch.override and ik_rematch.sd.rematch.victoryscreen>=1 then

local canRematch = (((player(1) and aiLevel()==0 and lose()) or (player(2) and aiLevel()==0 and lose()))  or (gameMode('watch') or gameMode('freebattle'))) and rematchmode()

if hv==0 and start.t_victory.counter - start.t_victory.textcnt >= ik_rematch.sd.victory_screen.time-5 then
hv = start.t_victory.counter- start.t_victory.textcnt

end



if canRematch and ik_rematch.sd.rematch.enabled>=1 and matchOver() then
for i =1,2 do
player(i)
mapSet('ik_rematch_on',1,'set')
end

end

if  ik_rematch.sd.rematch.enabled>=1 and matchOver() and ( roundState()==0 and motifState('victoryscreen'))  and (canRematch or map('ik_rematch')>0) and victoryend  then 



if not ik_rematch.rematchend() then
for i =1,2 do
player(i)
mapSet('ik_rematch',1,'set')
end

--lock victory counter
if hv>0 then
start.t_victory.counter = hv
end
end




if (player(1) and  map('ik_rematch')>0  and starttime==0 or (roundState()==-1 )) and (not ik_rematch.rematchend()) then
if not fade and (roundState()==-1  and starttime==0 ) then
--togglePause(true)
fade=true
end

	updatefont(ik_rematch.main.TextSpriteData,{
							font =   ik_rematch.sd.rematch.rematch.font[1],
							bank =   ik_rematch.sd.rematch.rematch.font[2],
							align =   ik_rematch.sd.rematch.rematch.font[3],
							text =  ik_rematch.sd.rematch.rematch.text,
							x =      rematchtxtpos()[1],
							y =      rematchtxtpos()[2],
							scaleX = ik_rematch.sd.rematch.rematch.scale[1],
							scaleY = ik_rematch.sd.rematch.rematch.scale[2],
							r =      ik_rematch.sd.rematch.rematch.font[4],
							g =      ik_rematch.sd.rematch.rematch.font[5],
							b =      ik_rematch.sd.rematch.rematch.font[6],
							layer= 2
						})

	textImgDraw(ik_rematch.main.TextSpriteData)
	for side =1,2 do
		if (player(side) and aiLevel()==0 or gameMode('watch')) then
				if not ik_rematch.done[side] then
					
				if getInput({side}, ik_rematch.sd.rematch.accept.key)  then
					sndPlay(motif.Snd, ik_rematch.sd.rematch['p'..side].done.snd[1], ik_rematch.sd.rematch['p'..side].done.snd[2])
					ik_rematch.done[side]=true
				end

				if (commandGetState(side, 'U')  or commandGetState(side, 'D'))  then
				
					sndPlay(motif.Snd, ik_rematch.sd.rematch['p'..side].cursor.snd[1], ik_rematch.sd.rematch['p'..side].cursor.snd[2])
					if ik_rematch.cursor[side] then
						ik_rematch.cursor[side]=false
					else
						ik_rematch.cursor[side]=true
					end
				end
					
		end

		if ik_rematch.cursor[side] then
			updatefont(txt_yes[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].active.font[1],
				bank =   ik_rematch.sd.rematch['p'..side].active.font[2],
				align =  ik_rematch.sd.rematch['p'..side].active.font[3],
				text =  ik_rematch.sd.rematch['p'..side].yes.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].active.font[4],
				g =      ik_rematch.sd.rematch['p'..side].active.font[5],
				b =      ik_rematch.sd.rematch['p'..side].active.font[6],
				layer= 2
				})
				
				updatefont(txt_no[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].font[1],
				bank =   ik_rematch.sd.rematch['p'..side].font[2],
				align =  ik_rematch.sd.rematch['p'..side].font[3],
				text =  ik_rematch.sd.rematch['p'..side].no.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				sx=rematchtxtpos(side)[2][1],
				sy=rematchtxtpos(side)[2][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].font[4],
				g =      ik_rematch.sd.rematch['p'..side].font[5],
				b =      ik_rematch.sd.rematch['p'..side].font[6],
				layer= 2
				})
			else
				updatefont(txt_yes[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].font[1],
				bank =   ik_rematch.sd.rematch['p'..side].font[2],
				align =  ik_rematch.sd.rematch['p'..side].font[3],
				text =  ik_rematch.sd.rematch['p'..side].yes.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].font[4],
				g =      ik_rematch.sd.rematch['p'..side].font[5],
				b =      ik_rematch.sd.rematch['p'..side].font[6],
				layer= 2
				})
				
				updatefont(txt_no[side].TextSpriteData,{
				font =   ik_rematch.sd.rematch['p'..side].active.font[1],
				bank =   ik_rematch.sd.rematch['p'..side].active.font[2],
				align =  ik_rematch.sd.rematch['p'..side].active.font[3],
				text =  ik_rematch.sd.rematch['p'..side].no.text,
				x =      rematchtxtpos(side)[1][1],
				y =      rematchtxtpos(side)[1][2],
				sx=rematchtxtpos(side)[2][1],
				sy=rematchtxtpos(side)[2][2],
				scaleX = ik_rematch.sd.rematch['p'..side].scale[1],
				scaleY = ik_rematch.sd.rematch['p'..side].scale[2],
				r =      ik_rematch.sd.rematch['p'..side].active.font[4],
				g =      ik_rematch.sd.rematch['p'..side].active.font[5],
				b =      ik_rematch.sd.rematch['p'..side].active.font[6],
				layer= 2
				})
			end
			textImgDraw(txt_yes[side].TextSpriteData)
			textImgDraw(txt_no[side].TextSpriteData)
					else
					    ik_rematch.done[side] =true
					end
					
		end
					
		if ik_rematch.rematchend() then
		togglePause(false)
		main.pauseMenu = false
		closeMenu()
		if ik_rematch.cursor[1] and ik_rematch.cursor[2] then
		toggleNoSound(false)
		start.bgmround=0
		reload()
		else
		for i =1,2 do
		player(i)
		mapSet('ik_rematch_on',0,'set')
		end
		end
		end
end

end
end
end
 hook.add("game.victory", "rematch2", ik_rematch.victory)

function ik_rematch.resetwincount()
ik_rematch.wincount={0,0}
end

hook.add("main.t_itemname", "rematch3", ik_rematch.resetwincount)
 hook.add("loop", "rematch", ik_rematch.run)
