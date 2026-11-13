package main

// The host layer's core: fighter slots, host commands, state records and events. Everything here is off unless
// ovConfigure (the overlay DLL) or IKEMEN_OVTEST (a scripted check in the normal exe) switches ovFFA on; with ovFFA
// false every engine hook below returns the stock answer.
//
// What the host layer changes in the engine (all behind ovFFA):
//   - 128 fighter slots (MaxSimul 64); a team per slot (ovTeamOf) instead of the two sides in every enemy, hit,
//     projectile, trade, push and p2defmul check;
//   - per fighter focus: p2 / enemy / enemynear = the host's focus (else the nearest enemy), numenemy 1, no partners,
//     win/lose false, so every fighter runs its own 1v1 code;
//   - an endless round (no time limit, no round end), fighters spawned and removed mid-fight;
//   - host input bits, forced CMD commands and z-walk per fighter; Pause/SuperPause scoped to the fighter and its
//     focus; EnvShake/EnvColor/AllPalFX exported as events.
//
// Host coordinates (every x / depth / height of a command, a state record and an event): a host unit is one pixel of
// a fighter whose localcoord width is config "ik_localcoord". x runs along the stage, depth into it, height up from
// the floor:
//   Ikemen x = x - origin_x,  Ikemen z = (depth - origin_depth) * depth_scale,  Ikemen y = -height
// With the defaults (origin_x 0, origin_depth 0, depth_scale 1) host x is the stage's own x (0 at its centre) and host
// depth its z.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ovFFA: the host layer's fighter-slot rules are on (set once, before the match boots)
var ovFFA bool

const ovREC = 32 // floats per fighter record (ikemen_state)

// The host coordinate mapping (config "origin_x", "origin_depth", "depth_scale"; see the head of this file)
var (
	ovFloorX0    float32     // host x of Ikemen x 0 (op "floor" moves it to the middle of its span)
	ovFloorD0    float32     // host depth of Ikemen z 0
	ovDepthScale float32 = 1 // Ikemen z per host depth unit
)

// ovFloorCommandValid: {"op": "floor", "x0", "x1"}: the floor's x span in host units (x0 < x1)
func ovFloorCommandValid(m map[string]interface{}) error {
	x0, ok0 := m["x0"].(float64)
	x1, ok1 := m["x1"].(float64)
	if !ok0 || !ok1 || math.IsNaN(x0) || math.IsNaN(x1) || math.IsInf(x0, 0) || math.IsInf(x1, 0) || x1 <= x0 {
		return fmt.Errorf("floor: x0 < x1 required (host units)")
	}
	return nil
}

// ovApplyFloor: the floor of the fight about to be set up (sent before its spawns): host x from x0 to x1. Its middle
// becomes Ikemen x 0 (origin_x) and the stage's left / right bounds - which alone hold fighters and projectiles here -
// become its two ends.
func ovApplyFloor(m map[string]interface{}) {
	x0, x1 := float32(m["x0"].(float64)), float32(m["x1"].(float64))
	half := (x1 - x0) / 2 * ovWPP() / sys.stage.localscl // host units -> stage units
	ovFloorX0 = (x0 + x1) / 2
	sys.stage.leftbound, sys.stage.rightbound = -half, half
	ovLog("floor: x %.0f..%.0f - Ikemen x 0 at %.0f, stage bounds %.0f..%.0f", x0, x1, ovFloorX0, sys.stage.leftbound,
		sys.stage.rightbound)
}

type ovSlot struct {
	used           bool    // reserved by spawn (or a boot fighter)
	ready          bool    // in the fight (event "spawned" sent)
	loading        bool
	preparing      bool    // compiled offscreen, not published until claimed by spawn
	prepareClaimed bool    // a spawn has reserved this preparation for activation
	cancel         bool    // despawned while loading
	sizeMul        float32 // its [Size] scale times this - 0 / 1: as loaded
	def            string
	pack           string // the pack's common-files folder (ovPackData: fightfx, common.snd)
	pal            int
	team           int32
	focus          int // handle or -1
	ai             int32
	bits           InputBits
	zmove          float32
	freeFace       bool             // host-steered facing: autoturn follows face, never the nearest enemy
	face           float32          // with freeFace: -1/1 the side to face, 0 keep the facing it has
	forced         map[string]int32 // CMD command name -> ticks left
	zAnim          bool             // showing the walk animation for a z-only walk
	lastHit        int              // last attacker handle or -1
	spawnAt        time.Time
	spawnMs        float64
	life           int32
	attack         float32
	defense        float32 // host factors, separate from the fighter's own AttackMulSet / DefenceMulSet
	motion         float32 // walk/run speed factor; never scales an attack's movement or animation time
	holdStart      int32   // a host control status uses the engine's pause of this root only
	holdUntil      int32
	replacing      bool // reserved for a spawn that will evict this corpse on the next tick
	revivePending  bool // a queued revive reserves this corpse against concurrent reclamation
	corpse         bool
	corpseAt       int32
	mute           bool // every sound of it and its helpers off
	pose           *ovPose
	x, depth       float32
	facing         float32
	// event edge detection
	wasAlive, deadSent bool
	// load result (set by the loader goroutine)
	loaded  bool
	loadErr error
	char    *Char
	ffx     *FightFx // the pack's fightfx + common.snd when it is not the motif's (committed into sys.ffx)
	ffxName string
}

type ovPose struct {
	kind      string
	t, most   int32
	idAtStart int32
	vars      map[int32]int32
	fvars     map[int32]float32
	explods   map[*Explod]bool // assets already active before the pose must keep their native lifetime
}

var ov struct {
	mu          sync.Mutex
	queue       []map[string]interface{}
	slots       [MaxPlayerNo]ovSlot
	slotCount   int
	events      []byte
	bootKeep    bool
	booted      bool
	pauseIn     [MaxPlayerNo]bool
	pauseOn     bool
	packFx      map[string]*FightFx // pack common-files dir (lower case) -> loaded fight fx
	packName    map[string]string   // pack dir -> its ffx prefix
	packReady   []string            // packs loaded since the last ovInstallPacks (their ffx names)
	loadSem     chan struct{}       // spawn loads running at once (config "load_workers", default 2)
	compileMu   sync.RWMutex        // state compiles share it (in parallel - ovCompileParallel); sys.ffx writes take it alone
	nearTick    [MaxPlayerNo]int32
	nearPn      [MaxPlayerNo]int
	tick        int32
	localcoord  float32 // the localcoord width whose pixels are the host units: config "ik_localcoord" (0: the stage's)
	intro       bool    // config "intro": the boot fighters play the round intro
	lifebar     bool    // config "lifebar": Ikemen draws its own lifebar
	corpseLimit int     // corpses retained, default 24; spawn pressure may evict sooner
}

func init() {
	ov.bootKeep = true
	ov.slotCount = MaxSimul * 2
	ov.corpseLimit = 24
	for i := range ov.slots {
		ov.slots[i].focus, ov.slots[i].lastHit = -1, -1
		ov.slots[i].team = ovDefaultTeam(i)
	}
	ov.packFx = map[string]*FightFx{}
	ov.packName = map[string]string{}
	ov.loadSem = make(chan struct{}, 2)
}

// ovSndLog: env IKEMEN_OV_SNDLOG=1 logs every fighter sound (char.go playSoundAt)
var ovSndLog = os.Getenv("IKEMEN_OV_SNDLOG") == "1"

// ovListenerH: the fighter sounds are panned around (op "listener"; -1: none - Ikemen's own panning)
var ovListenerH atomic.Int32

func init() { ovListenerH.Store(-1) }

// ovLog: through Ikemen's LogMessage (stderr; the DLL host sends stderr to save/logs/overlay.log)
func ovLog(format string, a ...interface{}) {
	LogMessage("[overlay] core %s: %s", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

// --- Configuration and boot ---

// ovConfigure: before boot, from the init config. Keys:
//   "ffa"           bool, default true: the fighter-slot rules of this file (false: a stock two-side match)
//   "boot_fighters" "keep" (default) | "disable": the match's boot fighters stay as slots 0.., or are hidden
//   "slots"         2..128, default 128: fighter slots
//   "ik_localcoord" > 0, default the stage's localcoord width (320 if unknown): a host unit is one pixel of a
//                   fighter with this localcoord width
//   "origin_x"      default 0: host x of Ikemen x 0
//   "origin_depth"  default 0: host depth of Ikemen z 0
//   "depth_scale"   > 0, default 1: Ikemen z per host depth unit
//   "intro"         bool, default false: the boot fighters play the round intro (false: they stand ready at once)
//   "lifebar"       bool, default false: Ikemen draws its own lifebar
//   "corpse_limit"  0..128, default 24: dead fighters kept in the fight
//   "load_workers"  1..4, default 2: fighters loading at once
func ovConfigure(cfg map[string]interface{}) {
	simConfigure(cfg)
	ovFFA = true
	if v, ok := cfg["ffa"].(bool); ok {
		ovFFA = v
	}
	if ovCfg.ParityMode {
		ovFFA = false // (the parity profile is a stock two-side match)
	}
	if v, ok := cfg["boot_fighters"].(string); ok {
		ov.bootKeep = v != "disable"
	}
	if v, ok := cfg["slots"].(float64); ok && v >= 2 && int(v) <= MaxSimul*2 {
		ov.slotCount = int(v)
	}
	if v, ok := cfg["ik_localcoord"].(float64); ok && v > 0 {
		ov.localcoord = float32(v)
	}
	if v, ok := cfg["origin_x"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
		ovFloorX0 = float32(v)
	}
	if v, ok := cfg["origin_depth"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
		ovFloorD0 = float32(v)
	}
	if v, ok := cfg["depth_scale"].(float64); ok && v > 0 && !math.IsInf(v, 0) {
		ovDepthScale = float32(v)
	}
	if v, ok := cfg["intro"].(bool); ok {
		ov.intro = v
	}
	if v, ok := cfg["lifebar"].(bool); ok {
		ov.lifebar = v
	}
	if v, ok := cfg["corpse_limit"].(float64); ok {
		ov.corpseLimit = int(Clamp(int32(v), 0, MaxSimul*2))
	}
	if v, ok := cfg["load_workers"].(float64); ok && v >= 1 && v <= 4 {
		ov.loadSem = make(chan struct{}, int(v))
	}
	ovConfigureMemory(cfg)
	ovLog("configured: ffa %v, boot_fighters keep %v, slots %v, sim_state_workers %d, defer helper %v, fast thread id %v",
		ovFFA, ov.bootKeep, ov.slotCount, simStateWorkers, simStateDefer && simDeferHelper, ovTidFast)
}

// ovBootSetup: inside the match start (after the round reset), before the first tick. Idempotent; resetRound calls
// it too. The round never ends (no time limit, no win count a host could reach) and, unless config "intro" is set, is
// treated as round 2+ with no intro: the boot fighters skip the round start and stand in state 0 with control. With
// "boot_fighters": "disable" they are hidden instead. Ikemen's lifebar is drawn only with config "lifebar".
func ovBootSetup() {
	if !ovFFA || ov.booted {
		return
	}
	ov.booted = true
	if !ov.intro {
		sys.roundNo = Max(sys.roundNo, 2)
		sys.intro = 0
	}
	sys.maxRoundTime = -1
	sys.curRoundTime = -1
	sys.matchWins = [2]int32{99, 99}
	sys.lifebarHide = !ov.lifebar
	// No team life or power sharing: the two internal sides are bookkeeping only (powerOwner would read slot 0/1)
	sys.cfg.Options.Team.PowerShare, sys.cfg.Options.Team.LifeShare = false, false
	for pn := 0; pn < MaxSimul*2; pn++ {
		if len(sys.chars[pn]) == 0 || sys.chars[pn][0] == nil {
			continue
		}
		c := sys.chars[pn][0]
		s := &ov.slots[pn]
		if !ov.bootKeep {
			c.setSCF(SCF_disabled | SCF_standby)
			c.teamside = -1
			continue
		}
		s.used, s.ready, s.def, s.char = true, true, sys.cgi[pn].def, c
		s.pack = ovPackDir(s.def)
		s.wasAlive = true
		s.attack, s.defense, s.motion = 1, 1, 1
		if !ov.intro { // (with the intro: the engine's own round start puts it into the fight)
			ovEnterFight(c, pn)
		}
		ovEvent(`{"e":"spawned","h":%d,"id":%d,"idle_anim":%v}`, pn, c.id, c.anim != nil)
	}
	sys.charList.enemyNearChanged = true
	ovLog("boot: round %d, intro %v, keep boot fighters %v", sys.roundNo, ov.intro, ov.bootKeep)
}

// ovEnterFight: a root goes straight into the fight as if round 2+ had just started (no round start states), in 0
// with control.
func ovEnterFight(c *Char, pn int) {
	c.unsetSCF(SCF_disabled | SCF_standby | SCF_ko | SCF_over_ko | SCF_over_alive)
	c.teamside = pn & 1
	// New late roots have animNo=0 but no Animation: use the native AIR/SFF path once; booted roots and existing
	// native animations remain intact.
	if c.anim == nil {
		c.changeAnim(0, pn, -1, "")
	}
	c.setCtrl(true)
	c.selfState(0, -1, -1, 1, "")
	for j := range c.cmd {
		c.cmd[j].BufReset()
	}
}

// ovCommand: every host op except pause, sound and the replay ops (host_ov_api.go). Parsed and checked here, applied
// on the Ikemen thread in ovBetweenTicks (spawn returns its handle at once).
func ovCommand(js []byte) (int, error) {
	var m map[string]interface{}
	if err := json.Unmarshal(js, &m); err != nil {
		return -1, err
	}
	op, _ := m["op"].(string)
	ov.mu.Lock()
	defer ov.mu.Unlock()
	switch op {
	case "prepare":
		return ovPrepareSpawn(m)
	case "prepared":
		return ovPreparedStatus(m), nil
	case "packs_ready":
		return ovPacksReady(), nil
	case "clear_prepared":
		return ovClearPrepared(), nil
	case "spawn":
		if ovCfg.ParityMode {
			return ovParitySpawn(m)
		}
		def, _ := m["def"].(string)
		if def == "" {
			return -1, fmt.Errorf("spawn: no def")
		}
		if _, err := os.Stat(ovPath(def)); err != nil {
			return -1, fmt.Errorf("spawn: %v", err)
		}
		for _, key := range []string{"attack", "defense"} {
			if v, present := m[key]; present && !ovValidMul(v) {
				return -1, fmt.Errorf("spawn: %s must be finite and > 0", key)
			}
		}
		if prepared := ovClaimPrepared(m); prepared >= 0 {
			return prepared, nil
		}
		pn := -1
		for i := 0; i < ov.slotCount; i++ {
			if !ov.slots[i].used && len(sys.chars[i]) == 0 {
				pn = i
				break
			}
		}
		if pn < 0 {
			pn = ovDiscardablePrepared()
			if pn < 0 {
				pn = ovOldestCorpse()
			}
			if pn < 0 {
				return -1, fmt.Errorf("spawn: no free slot (no corpse to evict)")
			}
			ov.slots[pn].replacing = true
			m["reuse"] = true
		} else {
			ovReserveSpawn(pn, m)
		}
		m["h"] = float64(pn)
		ov.queue = append(ov.queue, m)
		return pn, nil
	case "listener": // {"h": n}: sounds are panned around this fighter; -1 (or no "h"): Ikemen's own panning
		h := -1
		if v, ok := m["h"].(float64); ok && v >= 0 && int(v) < MaxSimul*2 {
			h = int(v)
		}
		ovListenerH.Store(int32(h))
		return 0, nil
	case "clear_leftovers":
		// every slot with no fighter in it is emptied - its explods, projectiles and texts; a slot with a fighter in
		// it keeps only what that fighter and its helpers made
		n := 0
		for pn := range sys.chars {
			if len(sys.chars[pn]) > 0 && sys.chars[pn][0] != nil {
				mine := map[int32]bool{}
				for _, c := range sys.chars[pn] {
					if c != nil {
						mine[c.id] = true
					}
				}
				for _, e := range sys.explods[pn] {
					if e != nil && e.id != IErr && !mine[e.ownerId] {
						e.flagForRemoval()
						n++
					}
				}
				continue
			}
			n += len(sys.explods[pn])
			sys.projs[pn] = PointerSliceReset(sys.projs[pn])
			sys.explods[pn] = PointerSliceReset(sys.explods[pn])
			sys.chartexts[pn] = PointerSliceReset(sys.chartexts[pn])
		}
		sys.explodDrawOrder = PointerSliceReset(sys.explodDrawOrder)
		ovLog("leftovers cleared: %d explods in empty slots", n) // (not ovEvent: it takes ov.mu, held here)
		return 0, nil
	case "corpse_limit":
		v, ok := m["limit"].(float64)
		if !ok || v != math.Trunc(v) || v < 0 || v > MaxSimul*2 {
			return -1, fmt.Errorf("corpse_limit: limit must be 0..128")
		}
		ov.queue = append(ov.queue, m)
		return 0, nil
	case "despawn", "input", "force_command", "focus", "face", "mute", "team", "ai", "set_pos", "nudge", "var", "life", "attack", "defense", "motion", "control", "pose", "revive", "palette", "size":
		h, ok := m["h"].(float64)
		if !ok || h != math.Trunc(h) || h < 0 || int(h) >= MaxSimul*2 || !ov.slots[int(h)].used {
			return -1, fmt.Errorf("%s: no fighter %v", op, m["h"])
		}
		if op == "palette" && !ovPaletteValid(m) { // {"pal": n}: its costume, live (host_ov_palette.go)
			return -1, fmt.Errorf("palette: pal must be 1..%d", sys.cfg.Config.PaletteMax)
		}
		if op == "attack" || op == "defense" || op == "motion" {
			if !ovValidMul(m["mul"]) {
				return -1, fmt.Errorf("%s: mul must be finite and > 0", op)
			}
		}
		if op == "control" {
			kind, _ := m["kind"].(string)
			if kind != "freeze" && kind != "stun" && kind != "launch" && kind != "knockdown" && kind != "knock" {
				return -1, fmt.Errorf("control: unsupported kind %q", kind)
			}
			t, ok := m["ticks"].(float64)
			if !ok || t != math.Trunc(t) || t < 1 || t > 180 {
				return -1, fmt.Errorf("control: ticks must be 1..180")
			}
		}
		if op == "pose" || op == "revive" {
			c := ovLive(int(h))
			if c == nil || !ov.slots[int(h)].ready || ov.slots[int(h)].replacing {
				return -1, fmt.Errorf("%s: fighter not ready", op)
			}
			if op == "revive" {
				if ov.slots[int(h)].revivePending {
					return -1, fmt.Errorf("revive: already queued")
				}
				if err := ovCanRevive(c); err != nil {
					return -1, err
				}
				v, ok := m["life"].(float64)
				if !ok || v < 1 || v > math.MaxInt32 || math.IsNaN(v) || math.IsInf(v, 0) {
					return -1, fmt.Errorf("revive: life must be >= 1")
				}
			} else if !c.alive() {
				return -1, fmt.Errorf("%s: fighter dead", op)
			}
			if op == "pose" {
				kind, _ := m["kind"].(string)
				if _, err := ovPoseState(c, kind); err != nil {
					return -1, err
				}
			}
		}
		if op == "input" { // (held until changed: applied at once, read every tick)
			s := &ov.slots[int(h)]
			if v, ok := m["bits"].(float64); ok {
				s.bits = InputBits(int32(v) & 0x3fff)
			}
			if v, ok := m["zmove"].(float64); ok {
				s.zmove = float32(math.Max(-1, math.Min(1, v)))
			}
			return 0, nil
		}
		if op == "mute" { // {"on": bool}: none of its sounds played (ovSlotMute)
			ov.slots[int(h)].mute, _ = m["on"].(bool)
			return 0, nil
		}
		if op == "face" { // {"free": bool, "dir": -1/0/1}: held like input, so a press and its turn land on one tick
			s := &ov.slots[int(h)]
			if v, ok := m["free"].(bool); ok {
				s.freeFace = v
			}
			s.face = 0
			if v, ok := m["dir"].(float64); ok && v != 0 {
				s.face = float32(math.Copysign(1, v))
			}
			return 0, nil
		}
		if op == "revive" {
			ov.slots[int(h)].revivePending = true
		}
		ov.queue = append(ov.queue, m)
		return 0, nil
	case "floor":
		if err := ovFloorCommandValid(m); err != nil {
			return -1, err
		}
		ov.queue = append(ov.queue, m)
		return 0, nil
	}
	return -1, fmt.Errorf("unknown op %q", op)
}

// ovHostInput: the held host input bits of a slot (absolute L/R), read by the engine's input update
func ovHostInput(pn int) (InputBits, bool) {
	if (!ovFFA && !ovCfg.ParityMode) || pn < 0 || pn >= MaxSimul*2 {
		return 0, false
	}
	s := &ov.slots[pn]
	if !s.used || !s.ready || s.ai != 0 {
		return 0, false
	}
	return s.bits, true
}

// ovFullCommandTicks: env IKEMEN_FORCE_COMMAND_TICKS != "0", read once (it was read every tick)
var ovFullCommandTicks = os.Getenv("IKEMEN_FORCE_COMMAND_TICKS") != "0"

// ovBetweenTicks: on the Ikemen thread right before action(): commit loaded spawns, apply queued commands, hold the
// host inputs and forced commands, z-walk, poses.
func ovBetweenTicks() {
	if ovCfg.ParityMode {
		ovParityBetweenTicks()
		return
	}
	if !ovFFA {
		return
	}
	// Age existing pulses before accepting this tick's requests: a host request for N ticks must get N active
	// ticks, including this one. Keep the ready/root guards, and age before committing a newly ready spawn
	// so a request waiting for that fighter does not lose its first evaluation either. Dev A/B: IKEMEN_FORCE_COMMAND_TICKS=0.
	fullCommandTicks := ovFullCommandTicks
	ageForced := func(s *ovSlot) {
		for name, t := range s.forced {
			if t <= 0 {
				delete(s.forced, name)
			} else {
				s.forced[name] = t - 1
			}
		}
	}
	ov.tick++
	if fullCommandTicks {
		for pn := 0; pn < MaxSimul*2; pn++ {
			s := &ov.slots[pn]
			if s.ready && len(sys.chars[pn]) > 0 {
				ageForced(s)
			}
		}
	}
	ovCommitSpawns()
	ov.mu.Lock()
	q := ov.queue
	ov.queue = nil
	ov.mu.Unlock()
	for _, m := range q {
		ovApply(m)
	}
	for pn := 0; pn < MaxSimul*2; pn++ {
		s := &ov.slots[pn]
		if !s.ready || len(sys.chars[pn]) == 0 {
			continue
		}
		c := sys.chars[pn][0]
		// Host input in the plain exe check (IKEMEN_OVTEST) through AssertInput's flag (cleared by the engine every
		// tick); in the DLL the engine's input update reads ovHostInput (full CMD buffering)
		if ovTestOn && s.ai == 0 && s.bits != 0 {
			c.inputFlag |= s.bits
		}
		if !fullCommandTicks {
			ageForced(s) // (legacy: a queued command loses its first tick before command evaluation)
		}
		ovZWalk(c, s)
		ovLazyCmd(c)
	}
	ovPosesStep()
	ovPruneCorpses()
	ovPauseScopeStep()
	ovMemoryTick()
}

// ovSpriteZ: a char's Ikemen z in host units (its sprite card's depth)
func ovSpriteZ(c *Char) float32 {
	if c == nil {
		return 0
	}
	return c.interPos[2] * c.localscl / ovWPP()
}

// --- Units ---

// ovWPP: Ikemen world units per host unit. A host unit is one pixel of a fighter whose localcoord width is config
// "ik_localcoord" - 320 by default.
func ovWPP() float32 {
	lc := ov.localcoord
	if lc <= 0 {
		lc = ovCfg.IkLocalcoord
		if lc <= 0 {
			lc = 320
		}
	}
	if sys.gameWidth <= 0 {
		return 320 / lc
	}
	return sys.gameWidth / lc
}

// host units -> a char's local units
func ovToLocal(c *Char, x, depth, height float32) (float32, float32, float32) {
	k := ovWPP() / c.localscl
	return (x - ovFloorX0) * k, -height * k, (depth - ovFloorD0) * ovDepthScale * k
}

// --- Spawn / despawn ---

func ovValidMul(v interface{}) bool {
	f, ok := v.(float64)
	return ok && float32(f) > 0 && f <= 1000 && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// Factors are inherited by helpers and applied to in-flight projectiles too. The fighter's own factors remain intact.
func ovDamageMul(attacker, victim *Char) float64 {
	if !ovFFA {
		return 1
	}
	a, d := float32(1), float32(1)
	if attacker != nil && attacker.playerNo >= 0 && attacker.playerNo < MaxSimul*2 && ov.slots[attacker.playerNo].attack > 0 {
		a = ov.slots[attacker.playerNo].attack
	}
	if victim != nil && victim.playerNo >= 0 && victim.playerNo < MaxSimul*2 && ov.slots[victim.playerNo].defense > 0 {
		d = ov.slots[victim.playerNo].defense
	}
	return float64(a) / float64(d)
}

func ovReserveSpawn(pn int, m map[string]interface{}) {
	def, _ := m["def"].(string)
	s := &ov.slots[pn]
	*s = ovSlot{used: true, loading: true, def: def, focus: -1, lastHit: -1, team: ovDefaultTeam(pn),
		pal: 1, facing: 1, attack: 1, defense: 1, motion: 1, x: ovFloorX0, depth: ovFloorD0, spawnAt: time.Now()}
	if v, ok := m["team"].(float64); ok {
		s.team = int32(v)
	}
	if v, ok := m["pal"].(float64); ok && v >= 1 {
		s.pal = int(v)
	}
	if v, ok := m["x"].(float64); ok {
		s.x = float32(v)
	}
	if v, ok := m["depth"].(float64); ok {
		s.depth = float32(v)
	}
	if v, ok := m["facing"].(float64); ok && v < 0 {
		s.facing = -1
	}
	if v, ok := m["ai"].(float64); ok {
		s.ai = Clamp(int32(v), 0, 8)
	}
	if v, ok := m["life"].(float64); ok && v > 0 {
		s.life = int32(v)
	}
	if v, ok := m["attack"].(float64); ok && ovValidMul(v) {
		s.attack = float32(v)
	}
	if v, ok := m["defense"].(float64); ok && ovValidMul(v) {
		s.defense = float32(v)
	}
	if v, ok := m["size"].(float64); ok && v > 0 {
		s.sizeMul = float32(math.Max(0.4, math.Min(2.5, v)))
	}
}

func ovOldestCorpse() int {
	oldest := -1
	for pn := 0; pn < ov.slotCount; pn++ {
		s := &ov.slots[pn]
		if s.ready && s.corpse && !s.replacing && !s.revivePending && (oldest < 0 || s.corpseAt < ov.slots[oldest].corpseAt) {
			oldest = pn
		}
	}
	return oldest
}

// ovDevCorpseLimit: (dev, env IKEMEN_CORPSE_LIMIT=<n>) the corpse limit whatever the host asks - for measurements only
var ovDevCorpseLimit = func() int {
	if n, err := strconv.Atoi(os.Getenv("IKEMEN_CORPSE_LIMIT")); err == nil && n >= 0 {
		return n
	}
	return -1
}()

func ovPruneCorpses() {
	limit := ov.corpseLimit
	if ovDevCorpseLimit >= 0 {
		limit = ovDevCorpseLimit
	}
	n := 0
	for pn := 0; pn < ov.slotCount; pn++ {
		if ov.slots[pn].corpse && !ov.slots[pn].replacing {
			n++
		}
	}
	for n > limit {
		pn := ovOldestCorpse()
		if pn < 0 {
			break
		}
		ovEvent(`{"e":"corpse_evicted","h":%d,"reason":"limit"}`, pn)
		ovDespawn(pn)
		n--
	}
}

func ovStateExists(c *Char, state int32) bool {
	if c == nil {
		return false
	}
	_, ok := sys.cgi[c.playerNo].states[state]
	return ok
}

// The standard pose states of the fighter's own compiled states, including character overrides.
func ovPoseState(c *Char, kind string) (int32, error) {
	states := []int32{}
	switch kind {
	case "win":
		states = []int32{180}
	case "taunt":
		states = []int32{195, 180} // a fighter with no taunt (195): its own victory gesture is the fallback
	case "intro":
		states = []int32{191, 190}
	default:
		return -1, fmt.Errorf("pose: kind must be win, taunt or intro")
	}
	for _, state := range states {
		if ovStateExists(c, state) {
			return state, nil
		}
	}
	return -1, fmt.Errorf("pose: no state for %s", kind)
}

// The endless round is round 2+ to skip the match's startup. A fighter in an "intro" pose (and its helpers) reads
// round 1, so its own first-round triggers apply; its stage/opponent/random conditions are untouched.
func ovRoundNo(c *Char) int32 {
	if ovFFA && c != nil && c.playerNo >= 0 && c.playerNo < MaxSimul*2 {
		if p := ov.slots[c.playerNo].pose; p != nil && p.kind == "intro" {
			return 1
		}
	}
	return sys.roundNo
}

func ovStartPose(c *Char, kind string) error {
	if c == nil || !c.alive() || ovHeld(c) || !c.ctrl() || c.ss.moveType == MT_H || c.ss.stateType == ST_A ||
		c.ss.sb.playerNo != c.playerNo || ov.slots[c.playerNo].pose != nil {
		return fmt.Errorf("pose: fighter must be grounded, in control and in its own states")
	}
	state, err := ovPoseState(c, kind)
	if err != nil {
		return err
	}
	p := &ovPose{kind: kind, most: 120, idAtStart: sys.lastCharId, vars: map[int32]int32{}, fvars: map[int32]float32{}, explods: map[*Explod]bool{}}
	if kind == "intro" {
		p.most = 300
	}
	for i, v := range c.cnsvar {
		if i < c.gi().data.intpersistindex {
			p.vars[i] = v
		}
	}
	for i, v := range c.cnsfvar {
		if i < c.gi().data.floatpersistindex {
			p.fvars[i] = v
		}
	}
	for _, e := range sys.explods[c.playerNo] {
		if e != nil && e.id != IErr {
			p.explods[e] = true
		}
	}
	ov.slots[c.playerNo].pose = p
	ov.slots[c.playerNo].bits, ov.slots[c.playerNo].zmove = 0, 0
	c.vel = [3]float32{}
	c.selfState(state, -1, -1, 0, "")
	ovEvent(`{"e":"pose_start","h":%d,"kind":%q,"state":%d}`, c.playerNo, kind, state)
	return nil
}

func ovPosesStep() {
	for pn := 0; pn < ov.slotCount; pn++ {
		s := &ov.slots[pn]
		p, c := s.pose, ovLive(pn)
		if p == nil || c == nil {
			continue
		}
		p.t++
		idle := c.ss.no == 0 || c.ss.no == 20 || c.ss.no == 21
		interrupted := !c.alive() || c.ss.moveType == MT_H
		done := idle && p.t > 2 || p.t >= p.most || c.ctrl() && p.t > 10 ||
			c.anim != nil && c.anim.loopend && p.t > 10
		if !done && !interrupted {
			continue
		}
		ovEndPose(c, p, interrupted)
	}
}

func ovEndPose(c *Char, p *ovPose, interrupted bool) {
	pn := c.playerNo
	// A native hit/custom state may already have changed combat variables. Do not roll that hit back when it
	// interrupts a pose; normal pose completion restores the round-only variables its controllers changed.
	if !interrupted {
		for i, v := range p.vars {
			c.varSet(i, v)
		}
		for i, v := range p.fvars {
			c.fvarSet(i, v)
		}
	}
	for _, h := range sys.chars[pn][1:] {
		if h != nil && h.id > p.idAtStart && !h.preserve {
			h.destroy()
		}
	}
	for _, e := range sys.explods[pn] {
		if e != nil && e.id != IErr && !p.explods[e] && e.time <= p.t &&
			(e.removetime < 0 || e.bindId == c.id && e.bindtime < 0) {
			e.id = IErr
		}
	}
	sys.explodPrune(pn)
	ov.slots[pn].pose = nil
	ovEvent(`{"e":"pose_end","h":%d,"kind":%q,"ticks":%d,"interrupted":%v}`, pn, p.kind, p.t, interrupted)
}

// ovHeld: the root's own pause holds it - a host control's freeze
func ovHeld(c *Char) bool {
	return ovFFA && c != nil && c.helperIndex == 0 && c.playerNo >= 0 && c.playerNo < MaxSimul*2 &&
		ov.tick >= ov.slots[c.playerNo].holdStart && ov.tick < ov.slots[c.playerNo].holdUntil
}

func ovMotion(c *Char) float32 {
	if ovFFA && c.helperIndex == 0 && c.playerNo >= 0 && c.playerNo < MaxSimul*2 &&
		c.ss.moveType != MT_A && c.ss.moveType != MT_H && (c.ss.no == 20 || c.ss.no == 21 || c.ss.no >= 100 && c.ss.no <= 107) {
		if v := ov.slots[c.playerNo].motion; v > 0 {
			return v
		}
	}
	return 1
}

// Host control effects enter the fighter's own common reactions. They never supply animation/sound/hitdef data.
// A freeze holds the current pose; a stun holds 5000 after its first controller tick; a launch uses 5030 and its
// usual fall/get-up. Foreign custom states are deliberately refused.
func ovControl(c *Char, m map[string]interface{}) error {
	if c == nil || !c.alive() || c.ss.sb.playerNo != c.playerNo || ov.slots[c.playerNo].pose != nil {
		return fmt.Errorf("control: only a living fighter in its own combat states is safe")
	}
	pn, kind, ticks := c.playerNo, m["kind"].(string), int32(m["ticks"].(float64))
	s := &ov.slots[pn]
	state := c.ss.no
	if kind == "freeze" {
		s.holdStart, s.holdUntil = ov.tick, Max(s.holdUntil, ov.tick+ticks)
	} else {
		if c.ss.stateType == ST_A || c.ss.stateType == ST_L {
			return fmt.Errorf("control: grounded reaction required")
		}
		c.gethitBindClear()
		c.resetBind()
		c.exitTarget()
		c.ghv.reset(c)
		c.unsetCSF(CSF_gethit)
		c.hitPauseTime = 0
		c.ghv.hittime, c.ghv.ctrltime = ticks, ticks
		c.ghv.animtype, c.ghv.groundanimtype, c.ghv.airanimtype = RA_Light, RA_Light, RA_Light
		c.ghv._type, c.ghv.groundtype, c.ghv.airtype = HT_High, HT_High, HT_High
		if by, ok := m["by"].(float64); ok {
			if attacker := ovLive(int(by)); attacker != nil && attacker.playerNo != pn {
				c.ghv.playerid, c.ghv.playerno = attacker.id, attacker.playerNo
				s.lastHit = attacker.playerNo
			}
		}
		state = 5000
		if kind == "stun" {
			s.holdStart, s.holdUntil = ov.tick+1, Max(s.holdUntil, ov.tick+ticks+1)
		} else {
			s.holdUntil = 0
			c.ghv.fallflag, c.ghv.fall_recover, c.ghv.fall_kill = true, false, false
			c.ghv.xvel = -2.5
			c.ghv.yvel = -3.5
			if kind == "launch" {
				c.ghv.yvel = -8
			}
			if kind == "knock" {
				c.ghv.xvel = -6
			}
			state = 5030
		}
		if !ovStateExists(c, state) {
			return fmt.Errorf("control: fighter has no state %d", state)
		}
		c.setCtrl(false)
		c.selfState(state, -1, -1, 0, "")
	}
	ovEvent(`{"e":"controlled","h":%d,"kind":%q,"state":%d,"ticks":%d}`, pn, kind, state, ticks)
	return nil
}

func ovCanRevive(c *Char) error {
	if c == nil {
		return fmt.Errorf("revive: no fighter")
	}
	if c.ss.sb.playerNo != c.playerNo {
		return fmt.Errorf("revive: a fighter in a foreign custom state cannot be revived")
	}
	if c.alive() || c.ss.no < 5000 || c.ss.no >= 5200 {
		return fmt.Errorf("revive: only a KO in own get-hit states (5000..5199) is safe")
	}
	return nil
}

func ovRevive(c *Char, life int32) error {
	if err := ovCanRevive(c); err != nil {
		return err
	}
	pn := c.playerNo
	s := &ov.slots[pn]
	c.exitTarget()
	c.gethitBindClear()
	c.resetBind()
	c.prepareNextRound()
	c.clearState()
	c.life = Clamp(life, 1, c.lifeMax)
	c.redLife = c.life
	c.koEchoTimer = 0
	c.setPosY(0, true)
	c.vel = [3]float32{}
	ovEnterFight(c, pn)
	s.wasAlive, s.deadSent = true, false
	s.corpse, s.corpseAt, s.pose = false, 0, nil
	s.holdUntil = 0
	s.bits, s.zmove, s.forced, s.lastHit = 0, 0, nil, -1
	sys.charList.enemyNearChanged = true
	ovEvent(`{"e":"revived","h":%d,"life":%d}`, pn, c.life)
	return nil
}

// ovPackData: the folder of a pack that holds its common files (fightfx.air/sff, common.snd, fight.def, system.def):
// <pack>/data itself, else the first folder under it that has them (packs that keep a motif in data/<name>). "" if none
func ovPackData(pack string) string {
	has := func(dir string) bool {
		for _, n := range []string{"fightfx.air", "fight.def", "system.def"} {
			if st, err := os.Stat(ovPath(filepath.Join(dir, n))); err == nil && !st.IsDir() {
				return true
			}
		}
		return false
	}
	data := filepath.Join(pack, "data")
	if has(data) {
		return data
	}
	subs, _ := filepath.Glob(filepath.Join(ovPath(data), "*"))
	for _, sub := range subs {
		if st, err := os.Stat(sub); err == nil && st.IsDir() && has(filepath.Join(data, filepath.Base(sub))) {
			return filepath.Join(data, filepath.Base(sub))
		}
	}
	return ""
}

// The common-files folder of a char def's pack: <pack>/chars/<char>/<file>.def -> ovPackData(<pack>)
func ovPackDir(def string) string {
	d := filepath.Dir(filepath.Clean(def))
	for i := 0; i < 4 && d != filepath.Dir(d); i++ {
		if strings.EqualFold(filepath.Base(d), "chars") {
			return ovPackData(filepath.Dir(d))
		}
		d = filepath.Dir(d)
	}
	return ""
}

// ovMotifFor: the motif search entry for a char's common files: its own pack's common-files folder (ovPackData)
func ovMotifFor(def string) string {
	if ovFFA {
		if p := ovPackDir(def); p != "" {
			return filepath.Join(p, "system.def")
		}
	}
	return sys.motif.Def
}

// ovSamePath: the same folder, through junctions and relative paths
func ovSamePath(a, b string) bool {
	real := func(p string) string {
		if abs, err := filepath.Abs(ovPath(p)); err == nil {
			p = abs
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return strings.ToLower(filepath.Clean(p))
	}
	return real(a) == real(b)
}

// ovApply: one queued command on the Ikemen thread
func ovApply(m map[string]interface{}) {
	op, _ := m["op"].(string)
	h := -1
	if v, ok := m["h"].(float64); ok {
		h = int(v)
	}
	var c *Char
	if h >= 0 && len(sys.chars[h]) > 0 {
		c = sys.chars[h][0]
	}
	s := (*ovSlot)(nil)
	if h >= 0 {
		s = &ov.slots[h]
	}
	switch op {
	case "activate_prepared":
		ovActivatePrepared(h, m)
	case "prepare":
		ovStartLoad(h, true)
	case "spawn":
		if reuse, _ := m["reuse"].(bool); reuse {
			ovEvent(`{"e":"corpse_evicted","h":%d,"reason":"slots"}`, h)
			ovDespawn(h)
			ovReserveSpawn(h, m)
		}
		ovStartLoad(h, false)
	case "corpse_limit":
		ov.corpseLimit = int(m["limit"].(float64))
	case "despawn":
		ovDespawn(h)
	case "force_command":
		name, _ := m["name"].(string)
		t := int32(1)
		if v, ok := m["ticks"].(float64); ok && v > 0 {
			t = int32(v)
		}
		if s.forced == nil {
			s.forced = map[string]int32{}
		}
		s.forced[name] = t
	case "focus":
		t := -1
		if v, ok := m["target"].(float64); ok && v >= 0 && int(v) < MaxSimul*2 {
			t = int(v)
		}
		s.focus = t
		sys.charList.enemyNearChanged = true
	case "team":
		if v, ok := m["team"].(float64); ok {
			s.team = int32(v)
		}
		sys.charList.enemyNearChanged = true
	case "ai":
		if v, ok := m["level"].(float64); ok {
			s.ai = Clamp(int32(v), 0, 8)
			if c != nil {
				c.setAILevel(float32(s.ai))
			}
		}
	case "size": // {"scale": n}: its [Size] scale times n, from now
		if v, ok := m["scale"].(float64); ok && v > 0 {
			s.sizeMul = float32(math.Max(0.4, math.Min(2.5, v)))
			if c != nil && s.ready {
				ovApplySize(c, s.sizeMul) // (live: at once; still loading, its commit puts it on - never on the empty placeholder)
			}
		}
	case "set_pos":
		if c == nil {
			return
		}
		x, _ := m["x"].(float64)
		d, _ := m["depth"].(float64)
		if !s.ready { // (still loading: where it will appear)
			s.x, s.depth = float32(x), float32(d)
			if f, ok := m["facing"].(float64); ok && f != 0 {
				s.facing = float32(math.Copysign(1, f))
			}
			return
		}
		hgt, _ := m["height"].(float64)
		lx, ly, lz := ovToLocal(c, float32(x), float32(d), float32(hgt))
		c.setPosX(lx, true)
		c.setPosY(ly, true)
		c.setPosZ(lz, true)
		c.vel = [3]float32{}
		if f, ok := m["facing"].(float64); ok && f != 0 {
			c.setFacing(float32(math.Copysign(1, f)))
		}
	case "nudge": // {"x", "depth"}: moved there along the floor - its height, its velocity and its state its own (set_pos
		// stops it dead and sets it down)
		if c == nil || !s.ready {
			return
		}
		x, okx := m["x"].(float64)
		d, okd := m["depth"].(float64)
		if !okx || !okd || math.IsNaN(x) || math.IsNaN(d) || math.IsInf(x, 0) || math.IsInf(d, 0) {
			return
		}
		lx, _, lz := ovToLocal(c, float32(x), float32(d), 0)
		c.setPosX(lx, true)
		c.setPosZ(lz, true)
	case "life": // {"value", "max", "by"}: value is at least 1 - only a hit knocks a fighter out
		if by, ok := m["by"].(float64); ok && by >= 0 && int(by) < MaxSimul*2 && int(by) != h {
			s.lastHit = int(by)
		}
		if v, ok := m["max"].(float64); ok && v >= 1 {
			if !s.ready {
				s.life = int32(v)
			} else if c != nil {
				c.lifeMax = int32(v)
			}
		}
		if v, ok := m["value"].(float64); ok && c != nil && s.ready && c.alive() {
			c.life = Clamp(int32(v), 1, Max(1, c.lifeMax))
			c.redLife = c.life
		}
	case "attack", "defense", "motion":
		mul := float32(m["mul"].(float64))
		if op == "attack" {
			s.attack = mul
		} else if op == "defense" {
			s.defense = mul
		} else {
			s.motion = mul
		}
	case "control":
		if err := ovControl(c, m); err != nil {
			ovEvent(`{"e":"command_error","op":"control","h":%d,"msg":%q}`, h, err.Error())
		}
	case "pose":
		if err := ovStartPose(c, m["kind"].(string)); err != nil {
			ovEvent(`{"e":"command_error","op":"pose","h":%d,"msg":%q}`, h, err.Error())
		}
	case "revive":
		s.revivePending = false
		if err := ovRevive(c, int32(m["life"].(float64))); err != nil {
			ovEvent(`{"e":"command_error","op":"revive","h":%d,"msg":%q}`, h, err.Error())
		}
	case "var":
		if c == nil {
			return
		}
		val, _ := m["value"].(float64)
		if i, ok := m["i"].(float64); ok {
			c.varSet(int32(i), int32(val))
		} else if f, ok := m["f"].(float64); ok {
			c.fvarSet(int32(f), float32(val))
		}
	case "floor":
		ovApplyFloor(m)
	case "palette":
		ovPaletteSet(h, c, s, int32(m["pal"].(float64)))
	}
}

// ovStartLoad: the hidden root goes into the slot now (skipped by every engine path while SCF_disabled), and the
// def loads and compiles on a goroutine (like BG Turns loading); ovCommitSpawns puts it into the fight
// The spawn loads' order: each load takes a ticket as it is asked for (on the Ikemen thread, in the host's order) and
// starts only when every earlier ticket has started: first asked, first loaded, still load_workers at once. A spawn's
// load goes before every waiting "prepare"; the prepares keep their own order behind them. Env IKEMEN_LOAD_FIFO=0: no order.
var ovLoadFifoOn = os.Getenv("IKEMEN_LOAD_FIFO") != "0"
var ovLoadQ struct {
	mu   sync.Mutex
	cond *sync.Cond
	next [2]int64 // tickets handed out: [0] spawns, [1] prepares
	head [2]int64 // ...and started
}

// ovLoadTicket: a load's place - `prepare` in the prepares' line, else the spawns'
func ovLoadTicket(prepare bool) (int, int64) {
	q := 0
	if prepare {
		q = 1
	}
	ovLoadQ.mu.Lock()
	defer ovLoadQ.mu.Unlock()
	t := ovLoadQ.next[q]
	ovLoadQ.next[q]++
	return q, t
}

// ovLoadTurn: (the load's goroutine) waits for every earlier ticket of its line to have started - a prepare also for
// every spawn asked for so far
func ovLoadTurn(q int, ticket int64) {
	if !ovLoadFifoOn {
		return
	}
	ovLoadQ.mu.Lock()
	if ovLoadQ.cond == nil {
		ovLoadQ.cond = sync.NewCond(&ovLoadQ.mu)
	}
	for ovLoadQ.head[q] < ticket || q == 1 && ovLoadQ.head[0] < ovLoadQ.next[0] {
		ovLoadQ.cond.Wait()
	}
	ovLoadQ.mu.Unlock()
}

// ovLoadStarted: the load holding the turn has its worker slot - the next ticket's turn
func ovLoadStarted(q int) {
	ovLoadQ.mu.Lock()
	ovLoadQ.head[q]++
	if ovLoadQ.cond != nil {
		ovLoadQ.cond.Broadcast()
	}
	ovLoadQ.mu.Unlock()
}

func ovStartLoad(pn int, prepare bool) {
	s := &ov.slots[pn]
	p := newChar(pn, 0)
	p.setSCF(SCF_disabled | SCF_standby)
	p.teamside = -1
	p.memberNo = -1
	if len(sys.chars[0]) > 0 && sys.chars[0][0] != nil {
		p.selectNo = sys.chars[0][0].selectNo
	}
	sys.aiLevel[pn] = 0
	sys.chars[pn] = []*Char{p}
	s.char = p
	def, motifDir := s.def, filepath.Dir(sys.motif.Def)
	line, ticket := ovLoadTicket(prepare)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ov.mu.Lock()
				s.loadErr, s.loaded = fmt.Errorf("load panic: %v", r), true
				ov.mu.Unlock()
			}
		}()
		t0 := time.Now()
		pack := ovPackDir(def)
		var ffx *FightFx
		var ffxName string
		// (the motif's own pack gets one too, outside the parity profile)
		withPack := pack != "" && (!ovSamePath(pack, motifDir) || !ovCfg.ParityMode)
		ovPreloadPacks(def) // (the first load starts every pack's fight fx in the background - ovPreloadPacks)
		ovLoadTurn(line, ticket) // (in the order they were asked for, spawns first - ovLoadTicket)
		if withPack && !ovPackLoaded(pack) {
			// (its pack's fight fx still loading: it waits for it holding neither a worker nor its place in the line)
			ovLoadStarted(line)
			ffx, ffxName = ovLoadPackFx(pack)
			ov.loadSem <- struct{}{}
		} else {
			ov.loadSem <- struct{}{} // (at most "load_workers" loads at once: CPU)
			ovLoadStarted(line)
			if withPack {
				ffx, ffxName = ovLoadPackFx(pack) // (loaded: at once)
			}
		}
		defer func() { <-ov.loadSem }()
		simAsyncLoadBegin() // (no SFF borrowing while this goroutine writes sys.cgi: image.go findActiveSff)
		defer simAsyncLoadEnd()
		t1 := time.Now()
		err := p.load(def)
		t2 := time.Now()
		if err == nil {
			func() { // (unlocked by defer: a compiler panic, recovered above, must not leave every later spawn waiting)
				// (a compile writes only its own slot's string pool and char info, and reads sys.ffx - whose writes take
				// the lock alone. IKEMEN_COMPILE_PARALLEL=0: one at a time)
				if ovCompileParallel {
					ov.compileMu.RLock()
					defer ov.compileMu.RUnlock()
				} else {
					ov.compileMu.Lock()
					defer ov.compileMu.Unlock()
				}
				sys.cgi[pn].states, err = newCharCompiler().Compile(pn, def, p.gi().constants)
			}()
		}
		t3 := time.Now()
		ov.mu.Lock()
		s.pack, s.ffx, s.ffxName = pack, ffx, ffxName
		s.loadErr, s.loaded = err, true
		ov.mu.Unlock()
		ovLog("loaded %s: pack fx %.0f ms, load %.0f ms, compile %.0f ms (err %v)", def,
			float64(t1.Sub(t0).Microseconds())/1000, float64(t2.Sub(t1).Microseconds())/1000,
			float64(t3.Sub(t2).Microseconds())/1000, err)
	}()
}

// ovLoadPackFx: a pack's fight fx (fightfx.sff/air, common.snd) as a char-independent FightFx, once per pack - a lock
// of its own a pack
func ovLoadPackFx(pack string) (*FightFx, string) {
	key := strings.ToLower(filepath.Clean(pack))
	lock, _ := ovPackLocks.LoadOrStore(key, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	ov.mu.Lock()
	ffx, name := ov.packFx[key], ov.packName[key]
	ov.mu.Unlock()
	if ffx != nil {
		return ffx, name
	}
	ffx = newFightFx()
	ov.compileMu.RLock() // (sys.ffx is written on the Ikemen thread under this lock: read under its shared side)
	if f := sys.ffx["f"]; f != nil {
		ffx.fx_scale, ffx.localcoord = f.fx_scale, f.localcoord
	}
	ov.compileMu.RUnlock()
	ffx.fileName = filepath.Join(pack, "fight.def")
	if s, err := loadSff(filepath.Join(pack, "fightfx.sff"), false, false, false); err == nil {
		*ffx.sff = *s
	} else {
		ovLog("pack fx %s: %v", pack, err)
	}
	if snd, err := LoadSnd(filepath.Join(pack, "common.snd")); err == nil {
		ffx.snd = snd
	} else {
		ovLog("pack common.snd %s: %v", pack, err)
	}
	if str, err := LoadText(filepath.Join(pack, "fightfx.air")); err == nil {
		lines, i := SplitAndTrim(str, "\n"), 0
		ffx.animTable = ReadAnimationTable(filepath.Join(pack, "fightfx.air"), ffx.sff, &ffx.sff.palList, lines, &i, false)
	}
	ffx.refCount = 99999
	ov.mu.Lock() // (the name numbered under the lock: two packs load at once)
	name = "hostfx" + strconv.Itoa(len(ov.packName)+1)
	ov.packFx[key], ov.packName[key] = ffx, name
	ov.packReady = append(ov.packReady, name)
	ov.mu.Unlock()
	return ffx, name
}

var ovPackLocks sync.Map // pack key -> *sync.Mutex
var ovCompileParallel = os.Getenv("IKEMEN_COMPILE_PARALLEL") != "0"

// ovPreloadPacks: every pack's fight fx read once in the background as Ikemen starts, never on a fighter's spawn
// mid-fight; `from` a fighter's def in one of them (the packs are the dirs beside its pack). Each pack its own
// goroutine. Env IKEMEN_PRELOAD_PACKS=0: each on its first fighter.
var ovPreloadPacksOnce sync.Once
var ovPreloadLeft atomic.Int32 // packs the preload has still to finish (0: none left, or none started)
var ovPreloadStarted atomic.Bool

func ovPreloadPacks(from string) {
	if os.Getenv("IKEMEN_PRELOAD_PACKS") == "0" || ovCfg.ParityMode || from == "" {
		return
	}
	ovPreloadPacksOnce.Do(func() {
		// the packs are the folders beside this fighter's pack: <packs>/<pack>/chars/<char>/<file>.def
		packRoot := ""
		for d, i := filepath.Dir(filepath.Clean(ovPath(from))), 0; i < 4 && d != filepath.Dir(d); d, i = filepath.Dir(d), i+1 {
			if strings.EqualFold(filepath.Base(d), "chars") {
				packRoot = filepath.Dir(d)
				break
			}
		}
		if packRoot == "" {
			return
		}
		var dirs []string
		siblings, _ := filepath.Glob(filepath.Join(filepath.Dir(packRoot), "*"))
		for _, sib := range siblings {
			if d := ovPackData(sib); d != "" {
				dirs = append(dirs, d)
			}
		}
		ovPreloadLeft.Store(int32(len(dirs)))
		ovPreloadStarted.Store(true)
		for _, d := range dirs { // (each pack its own goroutine and its own lock: in parallel)
			go func(d string) {
				defer ovPreloadLeft.Add(-1)
				defer func() { recover() }()
				t0 := time.Now()
				ovLoadPackFx(d)
				ovLog("preloaded pack fx %s in %.0f ms", d, float64(time.Since(t0).Microseconds())/1000)
			}(d)
		}
	})
}

// ovPacksReady: (op "packs_ready") 1 when every pack's fight fx the preload began is loaded (or there is no preload),
// 0 while one is still loading - a host loading screen waits on it
func ovPacksReady() int {
	if !ovPreloadStarted.Load() || ovPreloadLeft.Load() <= 0 {
		return 1
	}
	return 0
}

// ovPackLoaded: the pack's fight fx is in (ovLoadPackFx would return at once)
func ovPackLoaded(pack string) bool {
	key := strings.ToLower(filepath.Clean(pack))
	ov.mu.Lock()
	defer ov.mu.Unlock()
	return ov.packFx[key] != nil
}

// ovInstallPacks: (the Ikemen thread, each tick before the commits) the packs' fight fx loaded so far into sys.ffx -
// under the compile lock alone, so tried, never waited on: a compile running, the next tick
func ovInstallPacks() {
	ov.mu.Lock()
	ready := ov.packReady
	ov.mu.Unlock()
	if len(ready) == 0 || !ov.compileMu.TryLock() {
		return
	}
	n := 0
	ov.mu.Lock()
	for key, name := range ov.packName {
		if sys.ffx[name] == nil {
			ovSffBorrowMu.Lock()
			sys.ffx[name] = ov.packFx[key]
			ovSffBorrowMu.Unlock()
			n++
		}
	}
	ov.packReady = nil
	ov.mu.Unlock()
	ov.compileMu.Unlock()
	if n > 0 {
		ovLog("pack fx installed: %d", n)
	}
}


// ovAttachPackFx: (Ikemen's own loader, system.go - the match's boot fighters) c's own pack's fight fx as its "f", as
// ovStartLoad gives a spawned fighter its. Outside the parity profile only: there the match's fighters stay stock. A
// def's own fightfx.prefix stays.
func ovAttachPackFx(c *Char, def string) {
	if ovCfg.ParityMode || c == nil || c.gi().fightfxPrefix != "" {
		return
	}
	pack := ovPackDir(def)
	if pack == "" {
		return
	}
	ffx, name := ovLoadPackFx(pack)
	if ffx == nil || name == "" {
		return
	}
	// (into sys.ffx under the compile lock, as ovCommitSpawns puts it there: the loader goroutines read the map while
	// compiling)
	ov.compileMu.Lock()
	if sys.ffx[name] == nil {
		ovSffBorrowMu.Lock()
		sys.ffx[name] = ffx
		ovSffBorrowMu.Unlock()
	}
	ov.compileMu.Unlock()
	c.gi().fightfxPrefix = name
	ovLog("match fighter %s: its pack's fight fx %s (%s)", def, name, pack)
}

// ovCommitSpawns: loaded fighters into the fight (Ikemen thread, between ticks)
func ovCommitSpawns() {
	ovInstallPacks() // (the preloaded packs into sys.ffx first: their fighters commit with no lock - ovPreloadPacks)
	for pn := 0; pn < MaxSimul*2; pn++ {
		s := &ov.slots[pn]
		ov.mu.Lock()
		done := s.loading && s.loaded
		ov.mu.Unlock()
		if !done {
			continue
		}
		if s.preparing && !s.cancel && s.loadErr == nil {
			continue // stay disabled until a real spawn claims it
		}
		// The pack fx goes into sys.ffx, a map the loader goroutines read while compiling: only under the compile
		// lock; while a compile holds it, this fighter is committed next tick instead
		if s.ffx != nil && !s.cancel && s.loadErr == nil && sys.ffx[s.ffxName] == nil {
			// (only a pack not in sys.ffx yet takes the lock - ovInstallPacks puts the preloaded ones there: with the
			// compiles in parallel, a lock taken for every commit would wait out a whole burst of loads)
			if !ov.compileMu.TryLock() {
				continue
			}
			if sys.ffx[s.ffxName] == nil {
				if ovOn {
					ovSffBorrowMu.Lock()
				}
				sys.ffx[s.ffxName] = s.ffx
				if ovOn {
					ovSffBorrowMu.Unlock()
				}
			}
			ov.compileMu.Unlock()
		}
		s.loading = false
		p := s.char
		if s.cancel || s.loadErr != nil || p == nil || len(sys.chars[pn]) == 0 || sys.chars[pn][0] != p {
			if !s.cancel { // (the slot is free again from here on: the host drops or replaces that fighter at once)
				err := s.loadErr
				if err == nil {
					err = fmt.Errorf("its slot was taken while it loaded")
				}
				dj, _ := json.Marshal(s.def)
				ej, _ := json.Marshal(err.Error())
				ovEvent(`{"e":"spawn_failed","h":%d,"def":%s,"err":%s}`, pn, dj, ej)
				ovLog("spawn %d %s FAILED: %v", pn, s.def, err)
			}
			sys.chars[pn] = nil
			sys.cgi[pn].states = nil
			*s = ovSlot{focus: -1, lastHit: -1, team: ovDefaultTeam(pn)}
			continue
		}
		if s.ffx != nil {
			p.gi().fightfxPrefix = s.ffxName
		}
		sys.cgi[pn].palno = int32(s.pal)
		p.loadPalettes()
		p.id = sys.newCharId()
		sys.charList.add(p)
		// Command lists: its own (compiled); the other roots' copies of this slot are stale (lazy: ovLazyCmd)
		for i := range sys.chars {
			if ovOn && ovCfg.Pipeline && len(sys.chars[i]) > 0 && sys.chars[i][0] != nil && sys.chars[i][0].scf(SCF_disabled) {
				continue // compiler owns an unpublished root's command-list slice
			}
			if i != pn && len(sys.chars[i]) > 0 && sys.chars[i][0] != nil && len(sys.chars[i][0].cmd) > pn {
				sys.chars[i][0].cmd[pn].Names = map[string]int{}
				sys.chars[i][0].cmd[pn].Commands = nil
			}
		}
		// The round start for this fighter (system.go SetupCharRoundStart / resetRound, per char)
		p.prepareNextRound()
		// ...including its selected palette, as the stock round start applies it to a fighter joining the match
		// (system.go, roundsExisted == 0): the remap reset and, for an SFFv1 fighter, RemapPal 1,1 -> 1,palno.
		ovPaletteApply(p, false) // (and an SFFv2 fighter's costume too: host_ov_palette.go)
		lmax := float32(p.gi().data.life) * sys.cfg.Options.Life / 100
		if s.life > 0 {
			lmax = float32(s.life)
		}
		p.lifeMax = Max(1, int32(lmax))
		p.life, p.redLife = p.lifeMax, p.lifeMax
		p.power = 0
		p.dizzyPoints, p.guardPoints = p.dizzyPointsMax, p.guardPointsMax
		p.setAILevel(float32(s.ai))
		ovSizeBase.Delete(p)      // (its own [Size] as it loaded now - never the empty placeholder's zeros it was before)
		ovApplySize(p, s.sizeMul) // (bigger or smaller - op "size")
		lx, ly, lz := ovToLocal(p, s.x, s.depth, 0)
		p.setPosX(lx, true)
		p.setPosY(ly, true)
		p.setPosZ(lz, true)
		p.vel = [3]float32{}
		p.setFacing(s.facing)
		ovEnterFight(p, pn)
		s.ready, s.wasAlive, s.deadSent = true, true, false
		s.spawnMs = float64(time.Since(s.spawnAt).Microseconds()) / 1000
		sys.charList.enemyNearChanged = true
		ovEvent(`{"e":"spawned","h":%d,"id":%d,"idle_anim":%v,"ms":%.0f}`, pn, p.id, p.anim != nil, s.spawnMs)
	}
}

// ovDespawn: the fighter and everything it owns leave the fight; whoever was in its states, bound to it or focused on
// it is released
func ovDespawn(pn int) {
	s := &ov.slots[pn]
	ov.pauseIn[pn] = false // pause membership belongs to the removed root, not a future occupant of this slot
	if len(sys.chars[pn]) == 0 || sys.chars[pn][0] == nil {
		*s = ovSlot{focus: -1, lastHit: -1, team: ovDefaultTeam(pn)}
		return
	}
	root := sys.chars[pn][0]
	if !s.ready { // (still loading: ovCommitSpawns drops it when the load is done)
		root.setSCF(SCF_disabled | SCF_standby)
		s.cancel = true
		return
	}
	id := root.id
	ovReplayOwnerGone(id) // (replay: the texels of its recorded sprites are the replay's alone from now on)
	owned := map[int32]bool{}
	for _, c := range sys.chars[pn] {
		if c != nil {
			owned[c.id] = true
		}
	}
	helpers, explods, projs := 0, 0, 0
	released, foreignHelpers := 0, 0
	for _, h := range sys.chars[pn][1:] {
		if h != nil && !h.csf(CSF_destroy) {
			helpers++
		}
	}
	for _, e := range sys.explods[pn] {
		if e != nil && e.id != IErr {
			explods++
		}
	}
	for _, p := range sys.projs[pn] {
		if p != nil && p.id >= 0 {
			projs++
		}
	}
	for i := range sys.chars {
		if i == pn || !ov.slots[i].ready || len(sys.chars[i]) == 0 {
			continue
		}
		for _, o := range sys.chars[i] {
			if o == nil {
				continue
			}
			if owned[o.bindToId] {
				o.setBindTime(0)
				o.bindToId = -1
			}
			if o.ss.sb.playerNo == pn { // a victim/helper executing the removed fighter's custom states
				if o.helperIndex != 0 {
					o.destroy()
					foreignHelpers++
					continue
				}
				released++
				o.gethitBindClear()
				o.setCtrl(false)
				if o.alive() {
					o.selfState(0, -1, -1, 1, "")
				} else {
					o.selfState(5150, -1, -1, 0, "")
				}
			}
			for id := range owned {
				o.removeTarget(id)
				o.ghv.dropPlayerId(id)
			}
			if owned[o.ghv.playerid] {
				o.ghv.playerid, o.ghv.playerno = -1, -2
			}
			// A lazy command list belongs to the old root's generation; a replacement may have a different CMD.
			if pn < len(o.cmd) {
				o.cmd[pn].Commands, o.cmd[pn].Names = nil, nil
			}
		}
		for _, e := range sys.explods[i] {
			if e != nil && e.id != IErr && owned[e.bindId] {
				e.bindId, e.bindtime = -1, 0
			}
		}
		if ov.slots[i].focus == pn {
			ov.slots[i].focus = -1
		}
		if ov.slots[i].lastHit == pn {
			ov.slots[i].lastHit = -1
		}
	}
	for _, h := range sys.chars[pn][1:] {
		if h != nil {
			h.destroy()
		}
	}
	root.exitTarget()
	sys.clearPlayerAssets(pn, true)
	sys.removePlayerFromCharList(pn)
	sys.chars[pn] = nil
	sys.cgi[pn].states = nil
	sys.stringPool[pn].Clear()
	sys.aiLevel[pn] = 0
	sys.charList.enemyNearChanged = true
	*s = ovSlot{focus: -1, lastHit: -1, team: ovDefaultTeam(pn)}
	left, refs := 0, 0
	for _, c := range sys.charList.creationOrder {
		if c == nil {
			continue
		}
		if c.playerNo == pn {
			left++
		}
		if !c.csf(CSF_destroy) && c.ss.sb.playerNo == pn {
			refs++
		}
		if owned[c.bindToId] {
			refs++
		}
		for _, tid := range c.targets {
			if owned[tid] {
				refs++
			}
		}
		if owned[c.ghv.playerid] {
			refs++
		}
		for _, by := range c.ghv.targetedBy {
			if owned[by[0]] {
				refs++
			}
		}
	}
	for i := range sys.explods {
		for _, e := range sys.explods[i] {
			if e != nil && e.id != IErr && owned[e.bindId] {
				refs++
			}
		}
	}
	ovEvent(`{"e":"despawned","h":%d,"id":%d,"helpers":%d,"explods":%d,"projs":%d,"victims_released":%d,"foreign_helpers_removed":%d,"owned_left":%d,"references_left":%d}`,
		pn, id, helpers, explods, projs, released, foreignHelpers, left, refs)
}

// ovLazyCmd: a root running another slot's states (a custom-state victim) reads that slot's command list (the command
// trigger looks it up in the state owner's slot); copied the first time it is needed instead of N x N at load
func ovLazyCmd(c *Char) {
	o := c.ss.sb.playerNo
	if o == c.playerNo || o < 0 || o >= len(c.cmd) || len(c.cmd[o].Commands) > 0 || len(sys.chars[o]) == 0 ||
		sys.chars[o][0] == nil || len(sys.chars[o][0].cmd) <= o {
		return
	}
	c.cmd[o].CopyList(sys.chars[o][0].cmd[o])
}

// --- Teams, focus ---

// ovDefaultTeam: a slot's team until the host sets one: unique, and off the host's (non-negative) team numbers
func ovDefaultTeam(pn int) int32 {
	return -int32(pn) - 1
}

func ovTeamOf(pn int) int32 {
	if pn < 0 || pn >= len(ov.slots) {
		return -1
	}
	return ov.slots[pn].team
}

// ovSameSide: partners for push and SuperPause (overlay: the same FFA team; stock: the same teamside)
func ovSameSide(a, b *Char) bool {
	if ovFFA {
		return ovTeamOf(a.playerNo) == ovTeamOf(b.playerNo)
	}
	return a.teamside == b.teamside
}

// ovHitTeamDiffers: a HitDef's "other team" test (overlay: the attacker's FFA team; stock: the HitDef's teamside)
func ovHitTeamDiffers(getter *Char, hdTeamside int, attackerPn int) bool {
	if ovFFA {
		return ovTeamOf(getter.playerNo) != ovTeamOf(attackerPn)
	}
	return getter.teamside != hdTeamside
}

// ovMayHit: any two fighters may hit each other (their teams are tested apart: ovHitTeamDiffers)
func ovMayHit(attackerPn, getterPn int) bool {
	return true
}

func ovIsEnemy(c, e *Char) bool {
	if e.scf(SCF_disabled) || e.scf(SCF_standby) || e.teamside < 0 || e.playerNo == c.playerNo {
		return false
	}
	if e.helperIndex == 0 && !e.alive() {
		return false
	}
	return ovTeamOf(c.playerNo) != ovTeamOf(e.playerNo)
}

func ovLive(pn int) *Char {
	if pn < 0 || pn >= MaxSimul*2 || len(sys.chars[pn]) == 0 {
		return nil
	}
	c := sys.chars[pn][0]
	if c == nil || c.scf(SCF_disabled) || c.scf(SCF_standby) {
		return nil
	}
	return c
}

// ovFocus: the duel opponent of a char's fighter: the host's focus, else (no focus, or it left) the nearest enemy
// (x plus 4 x z, cached per tick)
func ovFocus(c *Char) *Char {
	if c.simCtx != nil && !c.vm().ctx.onEngine {
		if simFocusLog { // (dev: IKEMEN_SIM_FOCUS_LOG - who reads a focus in the pass)
			simStatsFocusRead(c)
		}
		return simOwn(c, simStates.focus[c.playerNo])
	}
	pn := c.playerNo
	if pn < 0 || pn >= MaxSimul*2 {
		return nil
	}
	if f := ovLive(ov.slots[pn].focus); f != nil && ovIsEnemy(c, f) {
		return f
	}
	if ov.nearTick[pn] == ov.tick && ov.tick != 0 {
		return ovLive(ov.nearPn[pn])
	}
	root := ovLive(pn)
	best, bd := -1, float32(math.MaxFloat32)
	if root != nil {
		for i := 0; i < MaxSimul*2; i++ {
			e := ovLive(i)
			if e == nil || !ovIsEnemy(root, e) {
				continue
			}
			dx := e.pos[0]*e.localscl - root.pos[0]*root.localscl
			dz := e.pos[2]*e.localscl - root.pos[2]*root.localscl
			d := float32(math.Hypot(float64(dx), float64(dz*4)))
			if d < bd {
				best, bd = i, d
			}
		}
	}
	ov.nearTick[pn], ov.nearPn[pn] = ov.tick, best
	return ovLive(best)
}

// ovFreeFace: autoturn of a host-steered root (op "face" free): it turns to the side the host gives and otherwise
// keeps its facing - never to the nearest enemy behind it. Every other fighter keeps the engine's own turn to p2.
// ok false: not free-facing, shouldFaceP2 decides.
func ovFreeFace(c *Char) (turn, ok bool) {
	pn := c.playerNo
	if !ovFFA || c.helperIndex != 0 || pn < 0 || pn >= MaxSimul*2 {
		return false, false
	}
	s := &ov.slots[pn]
	if !s.freeFace || s.ai != 0 {
		return false, false
	}
	return s.face != 0 && s.face != c.facing, true
}

// ovFreeBehind: p2 is behind a free-facing root - then it has no p2 (common stand/walk code turns to a p2 behind it
// again and again while it faces away; with none, its p2 triggers read false). Its enemy stays the focus.
func ovFreeBehind(c, p *Char) bool {
	if _, ok := ovFreeFace(c); !ok {
		return false
	}
	return (p.pos[0]*p.localscl-c.pos[0]*c.localscl)*c.facing < 0
}

// --- Commands ---

// ovForced: force_command makes the named command of the fighter's own CMD read as active
func ovForced(c *Char, pn, i int) bool {
	if c.helperIndex != 0 || c.playerNo < 0 || c.playerNo >= MaxSimul*2 {
		return false
	}
	f := ov.slots[c.playerNo].forced
	if len(f) == 0 {
		return false
	}
	cl := c.cmd[pn].At(i)
	return len(cl) > 0 && f[cl[0].name] > 0
}

// --- Z-walk ---

func ovZWalk(c *Char, s *ovSlot) {
	walking := s.zmove != 0 && c.ctrl() && c.alive() && c.ss.stateType == ST_S && !c.hitPause() && !ovHeld(c) &&
		(c.ss.no == 0 || c.ss.no == 20 || c.ss.no == 21)
	if walking {
		speed := c.gi().velocity.walk.fwd
		if speed <= 0 {
			speed = 2.4
		}
		motion := s.motion
		if motion <= 0 {
			motion = 1
		}
		z := c.pos[2] + s.zmove*speed*motion
		if sys.zmin != sys.zmax {
			z = Clamp(z, sys.zmin/c.localscl, sys.zmax/c.localscl)
		}
		c.setPosZ(z, false)
		if c.ss.no == 0 && c.ss.time > 0 && c.animNo != 20 && (c.animNo == 0 || s.zAnim) {
			c.changeAnim(20, c.playerNo, -1, "")
			s.zAnim = true
		}
		return
	}
	if s.zAnim {
		if c.ss.no == 0 && c.animNo == 20 {
			c.changeAnim(0, c.playerNo, -1, "")
		}
		s.zAnim = false
	}
}

// --- Pause / SuperPause scope ---

// ovPauseScope: a Pause or SuperPause freezes only its initiator's fighter and that fighter's focus
func ovPauseScope(c *Char) {
	if !ovFFA || c.playerNo < 0 || c.playerNo >= MaxSimul*2 {
		return
	}
	if !ov.pauseOn {
		ov.pauseIn = [MaxPlayerNo]bool{}
		ov.pauseOn = true
	}
	ov.pauseIn[c.playerNo] = true
	if f := ovFocus(c); f != nil {
		ov.pauseIn[f.playerNo] = true
	}
}

func ovPauseScopeStep() {
	if ov.pauseOn && sys.supertime <= 0 && sys.pausetime <= 0 && sys.supertimebuffer >= 0 && sys.pausetimebuffer >= 0 {
		ov.pauseOn = false
	}
}

// ovSuper / ovPause: the SuperPause / Pause timers as one slot sees them (out of the pause's scope: none)
func ovSuper(pn int) int32 {
	if ovFFA && ov.pauseOn && pn >= 0 && pn < len(ov.pauseIn) && !ov.pauseIn[pn] {
		return 0
	}
	return sys.supertime
}

func ovPause(pn int) int32 {
	if ovFFA && ov.pauseOn && pn >= 0 && pn < len(ov.pauseIn) && !ov.pauseIn[pn] {
		return 0
	}
	return sys.pausetime
}

// --- Sounds, global effects ---

// ovLogP2: (diagnostic, IKEMEN_OV_SNDLOG) the char a postype p2 helper or explod of c is placed at
func ovLogP2(c *Char, what string) {
	p2, r := c.p2(), ovLive(c.playerNo)
	name, x := "none", float32(0)
	if p2 != nil {
		name, x = p2.name, p2.pos[0]
	}
	rx := float32(0)
	if r != nil {
		rx = r.pos[0]
	}
	ovLog("%s at p2 of %s (state %d, root x %.0f): %s at x %.0f", what, c.name, c.ss.no, rx, name, x)
}

// ovSlotMute: c belongs to a fighter the host muted (op "mute")
func ovSlotMute(c *Char) bool {
	return c != nil && c.playerNo >= 0 && c.playerNo < MaxSimul*2 && ov.slots[c.playerNo].mute
}

func ovEnvShake(c *Char) {
	if !ovFFA {
		return
	}
	ovEvent(`{"e":"shake","h":%d,"ampl":%.2f,"time":%d,"freq":%.2f}`, c.playerNo, sys.envShake.ampl/ovWPP(),
		sys.envShake.time, sys.envShake.freq)
	sys.envShake.clear()
}

func ovEnvColor(c *Char) {
	if !ovFFA {
		return
	}
	ovEvent(`{"e":"envcolor","h":%d,"rgb":[%d,%d,%d],"time":%d,"under":%v}`, c.playerNo, sys.envcol[0], sys.envcol[1],
		sys.envcol[2], sys.envcol_time, sys.envcol_under)
	sys.envcol_time = 0
}

func ovAllPalFX(c *Char) {
	if !ovFFA {
		return
	}
	p := &sys.allPalFX.PalFXDef
	ovEvent(`{"e":"allpalfx","h":%d,"time":%d,"add":[%d,%d,%d],"mul":[%d,%d,%d],"color":%.2f}`, c.playerNo, p.time,
		p.add[0], p.add[1], p.add[2], p.mul[0], p.mul[1], p.mul[2], p.color)
	sys.allPalFX.clear()
}

// ovRumble: a ForceFeedback controller as an event for the host (it owns the pad): strong/weak motor 0..65535
func ovRumble(c *Char, self, newAPI bool, lo, hi uint16, ampl [4]float32, ticks uint32) {
	if !self {
		if c = c.p2(); c == nil {
			return
		}
	}
	strong, weak := int32(lo), int32(hi)
	if !newAPI {
		if c.helperIndex != 0 {
			return // Ikemen's own rule for the old API: "Don't rumble helpers" (bytecode.go forceFeedback.Run)
		}
		strong, weak = Clamp(int32(ampl[0]), 0, 255)*257, 0
	}
	ovEvent(`{"e":"rumble","h":%d,"strong":%d,"weak":%d,"ms":%d}`, c.playerNo, strong, weak, ticks*1000/60)
}

// --- Events ---

func ovEvent(format string, a ...interface{}) {
	if !ovFFA {
		return
	}
	ov.mu.Lock()
	ov.events = fmt.Appendf(ov.events, format, a...)
	ov.events = append(ov.events, '\n')
	if len(ov.events) > 4<<20 { // (nobody reads them: keep the newest)
		ov.events = ov.events[len(ov.events)-(2<<20):]
	}
	ov.mu.Unlock()
}

// Native helpers/projectiles keep their creating root state even after the root returns to idle.
// This is event attribution only: it never replaces the native state controller or HitDef.
func ovAttackOrigin(c *Char) int32 {
	if c.helperIndex != 0 {
		return c.ovOriginState
	}
	return c.ss.no
}

// ovHit: a hit lands (player or projectile) - event, last attacker
func ovHit(c, getter *Char, p *Projectile, result int32, damage int32) {
	if !ovFFA || getter == nil || c == nil {
		return
	}
	hd := &c.hitdef
	if p != nil {
		hd = &p.hitdef
	}
	guard := result == 2 || result == -2
	dmg := hd.hitdamage
	if guard {
		dmg = hd.guarddamage
	}
	if getter.playerNo >= 0 && getter.playerNo < MaxSimul*2 && getter.helperIndex == 0 && c.playerNo != getter.playerNo {
		ov.slots[getter.playerNo].lastHit = c.playerNo
	}
	rootState := c.ss.no
	if root := ovLive(c.playerNo); root != nil {
		rootState = root.ss.no
	}
	origin := ovAttackOrigin(c)
	if p != nil {
		origin = p.ovOriginState
	}
	ovEvent(`{"e":"hit","a":%d,"v":%d,"dmg":%d,"raw":%d,"guard":%v,"proj":%v,"ah":%v,"vh":%v,"state":%d,"root_state":%d,"origin_state":%d}`,
		c.playerNo, getter.playerNo, damage, dmg, guard, p != nil, c.helperIndex != 0, getter.helperIndex != 0, c.ss.no, rootState, origin)
}

// ovDrainEvents: the JSON lines since the last call (whole lines only; the rest stays for the next call)
func ovDrainEvents(buf []byte) int {
	ov.mu.Lock()
	defer ov.mu.Unlock()
	n := len(ov.events)
	if n > len(buf) {
		n = 0
		for n < len(ov.events) {
			i := n
			for i < len(ov.events) && ov.events[i] != '\n' {
				i++
			}
			if i+1 > len(buf) {
				break
			}
			n = i + 1
		}
	}
	copy(buf, ov.events[:n])
	ov.events = ov.events[:copy(ov.events, ov.events[n:])]
	return n
}

// ovAfterTick: after the tick's char logic: ko / dead / corpse events
func ovAfterTick() {
	if !ovFFA {
		return
	}
	for pn := 0; pn < MaxSimul*2; pn++ {
		s := &ov.slots[pn]
		if !s.ready {
			continue
		}
		c := ovLive(pn)
		if c == nil {
			continue
		}
		alive := c.alive()
		if s.wasAlive && !alive {
			ovEvent(`{"e":"ko","v":%d,"by":%d}`, pn, s.lastHit)
		}
		s.wasAlive = alive
		if !s.deadSent && !alive && (c.scf(SCF_over_ko) || c.ss.no == 5150 || c.ss.moveType != MT_H && c.life <= 0 && c.ss.time > 60) {
			s.deadSent = true
			ovEvent(`{"e":"dead","v":%d}`, pn)
		}
		if !s.corpse && s.deadSent {
			s.corpse, s.corpseAt = true, ov.tick
			ovEvent(`{"e":"corpse","h":%d,"id":%d,"tick":%d}`, pn, c.id, ov.tick)
		}
	}
}

// --- State records ---

// ovFillState: one record of ovREC (32) floats per fighter in the fight. Positions and sizes in host units (the head
// of this file), velocities in host units per tick.
//
//	 0 handle (the slot)          1 flags (below)             2 x                  3 depth
//	 4 height                     5 x velocity, forward +     6 depth velocity     7 height velocity, up +
//	 8 facing (1 right, -1 left)  9 life                     10 life max          11 power
//	12 state number              13 previous state number    14 time in the state
//	15 state type (0 stand, 1 crouch, 2 air, 3 lying)        16 move type (0 idle, 1 attack, 2 being hit)
//	17 animation number          18 focus handle (-1 none)   19 team              20 last attacker's handle (-1 none)
//	21 AI level                  22 body height              23 body width
//	24 host attack factor        25 host defense factor      26 1: a corpse
//	27 char id (tells successive fighters of one handle apart)
//	28 ticks left of a host freeze / stun                    29 host motion factor
//	30, 31 unused (0)
//
// Flags: 1 alive, 2 knocked out, 4 has control, 64 always set, 128 in hit pause, 256 in a guard state.
func ovFillState(buf []float32) int {
	n := 0
	wpp := ovWPP()
	for pn := 0; pn < MaxSimul*2; pn++ {
		s := &ov.slots[pn]
		if !s.ready || n+ovREC > len(buf) {
			continue
		}
		c := ovLive(pn)
		if c == nil {
			continue
		}
		r := buf[n : n+ovREC]
		for i := range r {
			r[i] = 0
		}
		k := c.localscl / wpp // local units -> host units
		var flags int32 = 64
		if c.alive() {
			flags |= 1
		} else {
			flags |= 2
		}
		if c.ctrl() {
			flags |= 4
		}
		if c.hitPause() {
			flags |= 128
		}
		if c.inGuardState() {
			flags |= 256
		}
		r[0] = float32(pn)
		r[1] = float32(flags)
		r[2] = c.pos[0]*k + ovFloorX0
		r[3] = c.pos[2]*k/ovDepthScale + ovFloorD0
		r[4] = -c.pos[1] * k
		r[5] = c.vel[0] * k * c.facing
		r[6] = c.vel[2] * k / ovDepthScale
		r[7] = -c.vel[1] * k
		r[8] = c.facing
		r[9] = float32(c.life)
		r[10] = float32(c.lifeMax)
		r[11] = float32(c.power)
		r[12] = float32(c.ss.no)
		r[13] = float32(c.ss.prevno)
		r[14] = float32(c.ss.time)
		switch c.ss.stateType {
		case ST_C:
			r[15] = 1
		case ST_A:
			r[15] = 2
		case ST_L:
			r[15] = 3
		}
		switch c.ss.moveType {
		case MT_A:
			r[16] = 1
		case MT_H:
			r[16] = 2
		}
		r[17] = float32(c.animNo)
		r[18] = -1
		if f := ovFocus(c); f != nil {
			r[18] = float32(f.playerNo)
		}
		r[19] = float32(s.team)
		r[20] = float32(s.lastHit)
		r[21] = float32(s.ai)
		r[22] = -c.sizeHeight[0] * k
		if r[22] < 0 {
			r[22] = -r[22]
		}
		r[23] = (c.sizeWidth[0] + c.sizeWidth[1]) * k
		r[24], r[25] = s.attack, s.defense
		if s.corpse {
			r[26] = 1
		}
		r[27] = float32(c.id) // distinguishes successive roots which reused this handle
		r[28], r[29] = float32(Max(int32(0), s.holdUntil-ov.tick)), s.motion
		n += ovREC
	}
	return n
}
