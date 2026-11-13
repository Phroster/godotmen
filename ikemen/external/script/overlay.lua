-- The host layer's boot script (Config.System inside ikemen.dll).
-- No menus: the quick-VS sequence of main.lua's f_commandLine with the fighters and the stage the host passed to
-- ikemen_init (Go sets the global hostOverlay = {fighters = {{def, pal, ai}, ...}, stage, roundTime, matchWins}),
-- then game(). Ikemen's match loop parks at every frame boundary until the host's next ikemen_step.

-- Common.Lua runs "loop()" every match frame (defaultConfig.ini); debug.lua is not loaded here
function loop() end

local cfg = hostOverlay
if cfg == nil or cfg.fighters == nil or #cfg.fighters < 2 then
	error("\nhostOverlay: at least 2 fighters needed\n")
end

setGameMode('quickvs')
setCredits(-1)
modifyGameOption('Config.BootLoadingMode', 0)
loadFightScreen()

-- Sides alternate: fighter 1 = P1 (side 1), fighter 2 = P2 (side 2), fighter 3 = P3 (side 1), ...
local numChars = {0, 0}
for i = 1, #cfg.fighters do
	local side = (i - 1) % 2 + 1
	numChars[side] = numChars[side] + 1
end
local teamMode = {0, 0}
for side = 1, 2 do
	if numChars[side] > 1 then
		teamMode[side] = 1 -- Simul
	end
	setMatchWins(side, cfg.matchWins or 99)
	setMatchMaxDrawGames(side, 99)
end

local frames = fightScreenVar("time.framespercount")
setTimeFramesPerCount(frames)
local roundTime = cfg.roundTime or -1
if roundTime > 0 then
	setRoundTime(roundTime * frames)
else
	setRoundTime(-1)
end

if addStage(cfg.stage) == 0 then
	error("\nhostOverlay: unable to add stage " .. tostring(cfg.stage) .. "\n")
end
clearSelected()
setMatchNo(1)
selectStage(1)
setTeamMode(1, teamMode[1], numChars[1])
setTeamMode(2, teamMode[2], numChars[2])

local ref = 0
for i, f in ipairs(cfg.fighters) do
	local side = (i - 1) % 2 + 1
	addChar(f.def)
	selectChar(side, ref, f.pal or 1)
	setCom(i, f.ai or 0)
	ref = ref + 1
end

loadStart('pausemenu=false')
-- (no busy "while loading()" spin: game() waits for the loader itself, through System.await, which sleeps 1 ms per
-- check while the overlay boots)
game()
