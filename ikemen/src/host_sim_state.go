package main

// The state pool executes the existing actionRun and bytecode interpreter. A
// component owns its characters and a VM. Shared controllers rendezvous with
// the engine thread only after every component has parked or finished.
import (
	"context"
	"fmt"
	"os"
	"strconv"
	"runtime/pprof"
	"path/filepath"
	"runtime"
	"sort"
	"sync/atomic"
	"time"
	"unsafe"
)

type simVM struct {
	bcStack, bcVarStack     BytecodeStack
	bcVar                   []BytecodeValue
	workingChar             *Char
	workingState            *StateBytecode
	workBe                  []BytecodeExp
	nest                    int32
	breakLoop, continueLoop bool
	ctx                     *simStateContext
}

var simStateWorkers = 4

// simStateDefer keeps the sound controllers (PlaySnd, StopSnd, stop on state
// change) and the cornerpush scan inside the component instead of parking it
// at the serial lane. Off = the original barriers (IKEMEN_SIM_STATE_DEFER=0).
var simStateDefer = true
var simDeferSound, simDeferCorner = true, true

// Kinds of shared operations completed without a barrier (diagnostics).
const (
	simLocalPlaySnd = iota
	simLocalStopSnd
	simLocalStateSound
	simLocalCornerPush
	simLocalHelper
	simLocalLifeSet
	simLocalEnvShake
	simLocalKinds
)

var simLocalNames = [simLocalKinds]string{"play_snd", "stop_snd", "state_sound", "corner_push", "helper", "life_set", "env_shake"}

func simLocalCounts() map[string]uint64 {
	m := make(map[string]uint64, simLocalKinds)
	for k, name := range simLocalNames {
		m[name] = simStates.local[k]
	}
	return m
}

var simSerialVM *simVM // accessed only by active characters at a quiescent barrier

func (c *Char) vm() *simVM {
	if c != nil && c.simCtx != nil {
		if simSerialVM != nil {
			return simSerialVM
		}
		return &c.simCtx.vm
	}
	return &sys.simVM
}

func (vm *simVM) random() int32 {
	if vm.ctx == nil {
		return Random()
	}
	s := &vm.ctx.seed
	w := *s / 127773
	*s = (*s-w*127773)*16807 - w*2836
	if *s <= 0 {
		*s += IMax - Btoi(*s == 0)
	}
	return *s
}

func (vm *simVM) rand(min, max int32) int32 {
	return min + vm.random()/(IMax/(max-min+1)+1)
}

func (vm *simVM) randI(x, y int32) int32 {
	if y < x {
		x, y = y, x
	}
	if uint32(y-x) > uint32(IMax) {
		return int32(int64(x) + int64(vm.random())*(int64(y)-int64(x))/int64(IMax))
	}
	return vm.rand(x, y)
}

func (vm *simVM) randF(x, y float32) float32 {
	return x + float32(vm.random())*(y-x)/float32(IMax)
}

// Tick/component IDs are mixed before the stock Park-Miller recurrence. No
// worker advances the stock seed. One-worker component mode uses these streams
// too; zero workers selects the original sequential FFA implementation.
func simComponentSeed(tick, id, seed int32) int32 {
	x := uint64(uint32(seed)) ^ uint64(uint32(tick))*0x9e3779b97f4a7c15 ^ uint64(uint32(id))*0xbf58476d1ce4e5b9
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	x ^= x >> 31
	return int32(x%uint64(IMax-1)) + 1
}

type simStateCall struct {
	fn        func()
	reply     chan struct{}
	exclusive bool
	failure   interface{}
	reason    string
}

type simStateNotice struct {
	ctx     *simStateContext
	call    *simStateCall
	failure interface{}
}

type simStateEffect struct {
	rank  int
	order int
	fn    func()
	kind  int8 // (dev stats) 0, or 1 + the simLocal kind of a commit
	batch bool // may run under a flush's held speaker lock (simSpeakerBatch): a sound channel change or a plain write
}

type simStateContext struct {
	vm                  simVM
	chars               []*Char
	ranks               []int
	rank                int
	seed                int32
	falseBlocks         uint64
	enemyChanged        bool
	effects             []simStateEffect
	order               int
	onEngine, exclusive bool
	soundPending        bool // a deferred PlaySnd of this component waits in effects
	shakePending        bool // a deferred EnvShake of this component waits in effects
	helperPending       []simPendingHelper // each deferred Helper creation of this component waiting in effects
	local               [simLocalKinds]uint64
	busyNs, parkNs      int64 // (dev: IKEMEN_SIM_STATS) this pass: running time and time parked at the serial lane
	job                 chan bool
	pool                *simStatePool
}

type simStatePool struct {
	contexts                     [MaxPlayerNo]*simStateContext
	tokens                       chan struct{}
	notices                      chan simStateNotice
	ticks, calls, exclusiveCalls uint64
	maxGroups, maxRoots          int
	focus                        [MaxPlayerNo]*Char
	controllerCalls              map[string]uint64
	barrierCalls                 map[string]uint64
	lastIDAtBegin                int32
	idsMonotonic                 bool
	comp                         [MaxPlayerNo]int // this tick's component of each player number
	local                        [simLocalKinds]uint64
	cost, lastCost               [MaxPlayerNo]int64 // each player's chars' running time: this pass's, the last pass's
	stamp                        [MaxPlayerNo]uint64
	order                        []*simStateContext // this pass's dispatch order (written before any job)
	est                          []int64
	next                         int32 // (atomic) the next component of order to start
	flushBuf                     []simStateEffect // (flush's kept buffer)
}

// simStateLPT: the components start largest first (their players' running time in the last pass), and only as
// many at once as there are workers, each next one when a running one parks or finishes - longest processing
// time first, so the largest component does not start last and end the pass alone. Only the order in which
// components start changes: every component's prefix is a function of the state at the pass's start and its own
// work (cross-component access parks), and the serial lane waits for all of them, so the results do not depend
// on it (the 1/4/6-worker proof). Env IKEMEN_SIM_LPT=0: the old dispatch (all at once, rank order).
var simStateLPT = os.Getenv("IKEMEN_SIM_LPT") != "0"

// admitNext: (a worker that just parked or finished, its token released) the next component in the dispatch order
// starts, if one is still waiting
func (p *simStatePool) admitNext() {
	if i := int(atomic.AddInt32(&p.next, 1)) - 1; i < len(p.order) {
		p.order[i].job <- true
	}
}

// sort.Interface over the dispatch order by estimated work, largest first (stable: rank order among equals)
type simByWork struct{ p *simStatePool }

func (b simByWork) Len() int           { return len(b.p.order) }
func (b simByWork) Less(i, j int) bool { return b.p.est[i] > b.p.est[j] }
func (b simByWork) Swap(i, j int) {
	b.p.order[i], b.p.order[j] = b.p.order[j], b.p.order[i]
	b.p.est[i], b.p.est[j] = b.p.est[j], b.p.est[i]
}

var simStates simStatePool

func (p *simStatePool) context(id int) *simStateContext {
	if p.notices == nil {
		p.notices = make(chan simStateNotice, MaxPlayerNo)
	}
	if p.contexts[id] != nil {
		return p.contexts[id]
	}
	x := &simStateContext{pool: p, job: make(chan bool)}
	x.vm.ctx = x
	p.contexts[id] = x
	go func() {
		for range x.job {
			p.tokens <- struct{}{}
			t0 := time.Now()
			func() {
				defer func() {
					failure := recover()
					x.busyNs = int64(time.Since(t0)) - x.parkNs
					<-p.tokens
					p.admitNext()
					p.notices <- simStateNotice{ctx: x, failure: failure}
				}()
				prev, parked := t0, int64(0)
				for i, c := range x.chars {
					x.rank = x.ranks[i]
					if simLabels {
						pprof.Do(context.Background(), simLabelOf(c), func(context.Context) { c.actionRun() })
					} else {
						c.actionRun()
					}
					// (simStateLPT: this char's running time, its serial-lane waits taken out, for the next
					// pass's dispatch order; every player belongs to one component, so no two workers write
					// the same entry)
					now := time.Now()
					p.cost[c.playerNo] += int64(now.Sub(prev)) - (x.parkNs - parked)
					prev, parked = now, x.parkNs
				}
			}()
		}
	}()
	return x
}

func (x *simStateContext) rendezvous(fn func(), exclusive bool, reason string) {
	if x.onEngine {
		if fn != nil {
			fn()
		}
		return
	}
	call := &simStateCall{fn: fn, reply: make(chan struct{}), exclusive: exclusive, reason: reason}
	t0 := time.Now()
	<-x.pool.tokens
	x.pool.admitNext()
	x.pool.notices <- simStateNotice{ctx: x, call: call}
	<-call.reply
	defer func() { x.parkNs += int64(time.Since(t0)) }() // (after the token is back)
	// Every served call flushed this component's effects (rank <= its own).
	x.helperPending = x.helperPending[:0]
	x.shakePending = false
	x.pool.tokens <- struct{}{}
	if call.failure != nil {
		panic(call.failure)
	}
}

func simOnEngine(c *Char, fn func()) {
	simOnEngineKind(c, fn, "shared")
}

func simOnEngineKind(c *Char, fn func(), reason string) {
	vm := c.vm()
	if vm.ctx == nil {
		fn()
		return
	}
	vm.ctx.rendezvous(fn, false, reason)
}

// An unanticipated cross-component redirect gets exclusive access before the
// returned pointer can be dereferenced. Peers remain parked for this component's
// continuation. Shared controllers still execute on the engine thread.
func simOwn(c, target *Char) *Char {
	if target != nil && c.simCtx != nil && target.simCtx != c.simCtx {
		x := c.vm().ctx
		if x != nil && !x.onEngine && !x.exclusive {
			reason := "foreign"
			if simStats != nil {
				reason = simCallSite("foreign")
			}
			x.rendezvous(nil, true, reason)
		}
	}
	return target
}

// Inspect immutable identity/ownership first. System.playerID also reads the
// target's mutable flags; those reads must follow the ownership barrier.
func (c *Char) simPlayerID(id int32) *Char {
	vm := c.vm()
	if vm.ctx == nil || vm.ctx.onEngine {
		return sys.playerID(id)
	}
	raw := sys.charList.idMap[id]
	if raw == nil {
		// Native helper IDs only advance during this joined pass. An absent ID
		// at/below its starting high-water mark cannot appear later in the pass.
		// Retain the barrier near int32 wrap and for genuinely future IDs.
		if id < 0 || simStates.idsMonotonic && id <= simStates.lastIDAtBegin {
			return nil
		}
		var result *Char
		simOnEngineKind(c, func() { result = sys.playerID(id) }, "missing_id")
		return simOwn(c, result)
	}
	simOwn(c, raw)
	return sys.playerID(id)
}

func (c *Char) simPlayerIDExist(id BytecodeValue) BytecodeValue {
	if id.IsUndefined() {
		return BytecodeUndefined()
	}
	return BytecodeBool(c.simPlayerID(id.ToI()) != nil)
}

func (c *Char) simPlayerIndexExist(id BytecodeValue) BytecodeValue {
	var result BytecodeValue
	simOnEngineKind(c, func() { result = sys.playerIndexExist(id) }, "index_exists")
	return result
}

func (c *Char) simPlayerNoExist(id BytecodeValue) BytecodeValue {
	var result BytecodeValue
	simOnEngineKind(c, func() { result = sys.playerNoExist(id) }, "player_exists")
	return result
}

func simStateEnabled() bool {
	return ovFFA && simStateWorkers > 0 && !sys.netplay() && !sys.cfg.Options.Team.LifeShare && !sys.cfg.Options.Team.PowerShare
}

func simRunStates(cl *CharList) bool {
	if !simStateEnabled() {
		return false
	}
	p := &simStates
	p.ticks++
	p.lastIDAtBegin = sys.lastCharId
	maxNewIDs := int64(MaxPlayerNo) * (int64(Max(int32(0), sys.cfg.Config.HelperMax)) + 1)
	p.idsMonotonic = sys.lastCharId >= 0 && int64(sys.lastCharId)+maxNewIDs < int64(IMax)
	// Prime every focus while all positions are stable. This also removes lazy
	// nearest-neighbour writes/scans from worker code.
	for i := range p.focus {
		p.focus[i] = nil
	}
	for _, c := range cl.runOrder {
		if !c.scf(SCF_disabled) {
			p.focus[c.playerNo] = ovFocus(c)
		}
	}
	g := simStateDependencyGraph()
	for pn := range p.comp {
		p.comp[pn] = g.find(pn)
	}
	count := len(cl.runOrder)
	active := make([]*simStateContext, 0, g.groups)
	for _, x := range p.contexts {
		if x != nil {
			x.chars = x.chars[:0]
			x.ranks = x.ranks[:0]
		}
	}
	var lowest [MaxPlayerNo]int32
	for i := range lowest {
		lowest[i] = IMax
	}
	for rank, c := range cl.runOrder {
		if c.scf(SCF_disabled) {
			continue
		}
		id := g.find(c.playerNo)
		x := p.context(id)
		if len(x.chars) == 0 {
			active = append(active, x)
		}
		x.chars = append(x.chars, c)
		x.ranks = append(x.ranks, rank)
		lowest[id] = Min(lowest[id], c.id)
		c.simCtx = x
	}
	if len(active) == 0 {
		return true
	}
	p.maxGroups = Max(p.maxGroups, len(active))
	p.maxRoots = Max(p.maxRoots, g.maxRoots)
	p.tokens = make(chan struct{}, simStateWorkers)
	for _, x := range active {
		x.onEngine = false
		x.exclusive = false
		x.falseBlocks = 0
		x.enemyChanged = false
		x.effects = x.effects[:0]
		x.order = 0
		x.soundPending = false
		x.shakePending = false
		x.helperPending = x.helperPending[:0]
		x.local = [simLocalKinds]uint64{}
		x.busyNs, x.parkNs = 0, 0
		x.seed = simComponentSeed(int32(sys.tickCount), lowest[g.find(x.chars[0].playerNo)], sys.randseed)
	}
	// Clear transient ownership even when a worker panics. Worker failures are
	// collected only after all peers finish, so DLL recovery can safely proceed.
	defer func() {
		simSerialVM = nil
		for _, c := range cl.runOrder {
			c.simCtx = nil
		}
		for _, x := range active {
			for i := range x.chars {
				if c := x.chars[i]; c != nil {
					c.simCtx = nil
				}
				x.chars[i] = nil
			}
			x.chars = x.chars[:0]
			x.ranks = x.ranks[:0]
			for i := range x.effects {
				x.effects[i] = simStateEffect{}
			}
			x.effects = x.effects[:0]
			x.vm.workingChar = nil
			x.vm.workingState = nil
		}
	}()
	// The dispatch order (simStateLPT): each component's estimate is its players' running time in the last pass.
	p.lastCost, p.cost = p.cost, [MaxPlayerNo]int64{}
	p.order = append(p.order[:0], active...)
	p.est = p.est[:0]
	for _, x := range p.order {
		var e int64
		for _, c := range x.chars { // (each player once)
			if p.stamp[c.playerNo] != p.ticks {
				p.stamp[c.playerNo] = p.ticks
				e += p.lastCost[c.playerNo]
			}
		}
		p.est = append(p.est, e)
	}
	start := len(p.order)
	if simStateLPT {
		sort.Stable(simByWork{p})
		start = Min(start, simStateWorkers)
	}
	t0 := time.Now() // (env IKEMEN_SIM_STATS: the pass's wall time)
	// Every component parks or finishes exactly once before the first serial turn: len(active) notices end the
	// parallel phase. The first `start` start now; each one that parks or finishes starts the next itself
	// (admitNext), so no start waits for the engine thread.
	atomic.StoreInt32(&p.next, int32(start))
	for i := 0; i < start; i++ {
		p.order[i].job <- true
	}
	var tPar time.Duration
	pending := make(map[*simStateContext]*simStateCall, len(active))
	running, finished := len(active), 0
	var failure interface{}
	for finished < len(active) {
		for running > 0 {
			n := <-p.notices
			running--
			if n.call != nil {
				pending[n.ctx] = n.call
			} else {
				finished++
				if n.failure != nil && failure == nil {
					failure = n.failure
				}
			}
		}
		simSerialVM = nil
		if tPar == 0 {
			tPar = time.Since(t0) // (the parallel phase: every component parked or finished once)
		}
		if len(pending) == 0 {
			break
		}
		var next *simStateContext
		for x := range pending {
			if next == nil || x.rank < next.rank {
				next = x
			}
		}
		call := pending[next]
		delete(pending, next)
		p.calls++
		if simProfiler != nil {
			if p.barrierCalls == nil {
				p.barrierCalls = make(map[string]uint64)
			}
			p.barrierCalls[call.reason]++
		}
		if simStats != nil {
			simStats.barriers[call.reason]++
		}
		// Parked peers must always receive a reply: a failing ordered effect
		// is recorded and raised only after every component has joined.
		if e := p.safeFlush(active, next.rank); e != nil && failure == nil {
			failure = e
		}
		simSerialVM = &next.vm
		if call.exclusive {
			next.exclusive = true
			p.exclusiveCalls++
		} else {
			next.onEngine = true
			func() {
				defer func() {
					if e := recover(); e != nil {
						call.failure = e
						failure = e
					}
				}()
				call.fn()
			}()
			next.onEngine = false
			if !next.exclusive {
				simSerialVM = nil
			}
		}
		close(call.reply)
		running = 1
	}
	simSerialVM = nil
	if failure != nil {
		panic(fmt.Sprintf("FFA state worker: %v", failure))
	}
	tFlush := time.Now()
	p.flush(active, int(^uint(0)>>1))
	tNew := time.Now()
	for _, x := range active {
		if x.enemyChanged {
			cl.enemyNearChanged = true
		}
	}
	// Native helpers appended by engine-thread controllers still run their first
	// states during this tick, in the original global append order.
	for i := count; i < len(cl.runOrder); i++ {
		c := cl.runOrder[i]
		if x := c.simCtx; x != nil {
			x.onEngine = true
			simSerialVM = &x.vm
			c.actionRun()
			x.onEngine = false
			simSerialVM = nil
		} else {
			c.actionRun()
		}
	}
	tEnd := time.Now()
	for _, x := range active {
		simCNSFalseBlocks += x.falseBlocks
		for k, n := range x.local {
			p.local[k] += n
		}
	}
	for _, c := range cl.runOrder {
		c.simCtx = nil
	}
	if simStats != nil {
		var busy, most int64
		for _, x := range active {
			busy += x.busyNs
			most = Max(most, x.busyNs)
		}
		simStats.phases(tPar, busy, most)
		simStats.tail(tNew.Sub(tFlush), tEnd.Sub(tNew), len(cl.runOrder)-count)
		simStats.pass(&g, len(active), count, time.Since(t0))
	}
	return true
}

// Shared operations use the serial lane by default. These controllers touch
// only their character/component; expression redirects are checked by simOwn.
func simRunController(sc StateController, c *Char, ps []int32) bool {
	if c.simCtx == nil || c.vm().ctx.onEngine {
		return sc.Run(c, ps)
	}
	if simStateDefer && simDeferSound {
		switch sc.(type) {
		case playSnd, stopSnd:
			// Parameters are evaluated here; the channel work is an ordered
			// effect (simPlaySnd, simSoundEffect).
			return sc.Run(c, ps)
		}
	}
	if simStateDefer && simDeferShake {
		if sc, ok := sc.(envShake); ok && ovFFA && simParamsLocal(StateControllerBase(sc)) {
			simEnvShake(sc, c)
			return false // (envShake.Run's result)
		}
	}
	if simStateDefer && simProjLocal {
		if _, ok := sc.(projectile); ok {
			// simProjLocal: Projectile in the component. It makes
			// a projectile in the redirected char's own player's list (sys.projs[pn]: in the pass only that
			// player's chars - its component - read or write it: NumProj/ProjHit..., ModifyProjectile via
			// getMultipleProjs; the projectile pass and hit detection run after the join), from that char's own
			// state, its primed focus (postype p2) and immutable data, through the same hitdef/afterimage code the
			// allowlisted HitDef and AfterImage run; a foreign redirect takes simOwn's exclusive turn first. No
			// global is written (its console warnings are ordered effects). IKEMEN_SIM_PROJ=0: the serial lane.
			return sc.Run(c, ps)
		}
	}
	if simStateDefer && simDeferHelper {
		if _, ok := sc.(helper); ok {
			// Deferred creation or its own barrier (simHelper).
			return sc.Run(c, ps)
		}
	}
	switch sc.(type) {
	case StateBlock, compiledStateBlock, *simCNSGroup, NullStateController, StateExpr,
		varAssign, LoopBreak, LoopContinue, CallFunction,
		varSet, varRangeSet, ctrlSet, velSet, velAdd, velMul, posSet, posAdd,
		changeState, selfState, changeAnim, changeAnim2, sprPriority, turn,
		hitDef, hitBy, hitOverride, notHitBy, width, screenBound, playerPush,
		attackMulSet, defenceMulSet, gravity, offset, stateTypeSet, angleDraw, angleSet,
		angleAdd, angleMul, trans, palFX, assertSpecial, varRandom, powerAdd, powerSet,
		hitVelSet, posFreeze, attackDist, moveHitReset, bindToParent, bindToRoot,
		height, depth, dizzyPointsAdd, dizzyPointsSet, guardPointsAdd, guardPointsSet,
		dizzySet, guardBreakSet, victoryQuote, mapSet, displayToClipboard,
		appendToClipboard, clearClipboard, removeExplod, explodBindTime,
		explod, modifyExplod, gameMakeAnim, remapPal, remapSprite, afterImage, afterImageTime,
		makeDust, reversalDef, destroySelf, hitFallVel, hitFallSet, hitFallDamage, lifeAdd, lifeSet,
		targetFacing, targetBind, bindToTarget, targetState, targetVelSet, targetVelAdd, targetDrop,
		targetLifeAdd, targetPowerAdd, targetDizzyPointsAdd, targetGuardPointsAdd, targetRedLifeAdd:
		return sc.Run(c, ps)
	default:
		var result bool
		simOnEngineKind(c, func() {
			if simProfiler != nil {
				if simStates.controllerCalls == nil {
					simStates.controllerCalls = make(map[string]uint64)
				}
				simStates.controllerCalls[fmt.Sprintf("%T", sc)]++
			}
			if simStats != nil {
				simStats.controllers[fmt.Sprintf("%T", sc)]++
			}
			result = sc.Run(c, ps)
		}, "controller")
		return result
	}
}

func simCountFalseCNS(c *Char, n uint64) {
	if x := c.vm().ctx; x != nil {
		x.falseBlocks += n
	} else {
		simCNSFalseBlocks += n
	}
}

func simEnemyNearChanged(c *Char) {
	if x := c.vm().ctx; x != nil {
		x.enemyChanged = true
	} else {
		sys.charList.enemyNearChanged = true
	}
}

func (c *Char) simConsole(s string) {
	fn := func() { sys.appendToConsole(s) }
	if x := c.vm().ctx; x != nil && !x.onEngine {
		x.deferEffect(fn)
		x.effects[len(x.effects)-1].batch = true // (the console's own leaf mutex only)
		return
	}
	fn()
}

func simEffect(c *Char, fn func()) {
	if x := c.vm().ctx; x != nil && !x.onEngine {
		x.deferEffect(fn)
	} else {
		fn()
	}
}

func (vm *simVM) printBytecodeError(s string) {
	if vm.ctx == nil {
		sys.printBytecodeError(s)
		return
	}
	c := vm.workingChar
	if c != nil {
		c.simConsole(c.warn() + s)
	}
}

func simSetGSF(c *Char, flag GlobalSpecialFlag, enabled bool) {
	fn := func() {
		if enabled {
			sys.setGSF(flag)
		} else {
			sys.unsetGSF(flag)
		}
	}
	if x := c.vm().ctx; x != nil && !x.onEngine {
		x.deferEffect(fn)
		x.effects[len(x.effects)-1].batch = true // (plain writes)
	} else {
		fn()
	}
}

func simReadGSF(c *Char, flag GlobalSpecialFlag) bool {
	var result bool
	simOnEngineKind(c, func() { result = sys.gsf(flag) }, "global_flag")
	return result
}

func (x *simStateContext) deferEffect(fn func()) {
	x.order++
	x.effects = append(x.effects, simStateEffect{rank: x.rank, order: x.order, fn: fn})
}

func (p *simStatePool) safeFlush(active []*simStateContext, rank int) (failure interface{}) {
	defer func() { failure = recover() }()
	p.flush(active, rank)
	return nil
}

func (p *simStatePool) flush(active []*simStateContext, rank int) {
	// (the effects are gathered in the pool's kept buffer - the same elements appended in the same
	// order as into a new slice. Taken out of the pool while in use, so a flush inside an effect would gather into a
	// new slice of its own as before; given back cleared - no effect's closure kept alive)
	effects := p.flushBuf[:0]
	p.flushBuf = nil
	defer func() {
		for i := range effects {
			effects[i] = simStateEffect{}
		}
		p.flushBuf = effects[:0]
	}()
	for _, x := range active {
		keep := x.effects[:0]
		for _, e := range x.effects {
			if e.rank <= rank {
				effects = append(effects, e)
			} else {
				keep = append(keep, e)
			}
		}
		for i := len(keep); i < len(x.effects); i++ {
			x.effects[i] = simStateEffect{}
		}
		x.effects = keep
	}
	sort.Slice(effects, func(i, j int) bool {
		a, b := effects[i], effects[j]
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.order < b.order
	})
	// simSpeakerBatch: a run of sound effects (with the plain writes between them) takes the speaker lock once - each
	// sound channel change otherwise waited for the host mixer's lock on its own (~50-100 us a PlaySnd in a large fight).
	held := false
	var heldAt time.Time
	release := func() {
		simSpeakerThread.Store(0)
		speaker.Unlock()
		runtime.UnlockOSThread()
		held = false
	}
	defer func() {
		if held {
			release()
		}
	}()
	for _, e := range effects {
		if held && (!e.batch || time.Since(heldAt) > simSpeakerBatchMax) { // (the mixer never shut out for a whole flush)
			release()
		} else if !held && e.batch && e.kind != 0 && simSpeakerBatch && speaker != nil {
			// (pinned to its OS thread while it holds the lock: the thread ID is how WithSpeakerLock knows it - the
			// engine thread is pinned already, a test's goroutine is not)
			runtime.LockOSThread()
			if tid := ovCurrentThreadID(); tid != 0 {
				speaker.Lock()
				simSpeakerThread.Store(tid)
				held, heldAt = true, time.Now()
			} else {
				runtime.UnlockOSThread()
			}
		}
		if simStats != nil { // (dev: IKEMEN_SIM_STATS - the flush's time by kind of effect)
			t := time.Now()
			e.fn()
			simStats.flushKind[e.kind] += int64(time.Since(t))
			simStats.flushKindN[e.kind]++
			continue
		}
		e.fn()
	}
}

// simSpeakerThread: the OS thread whose flush holds the speaker lock now (0: none) - WithSpeakerLock on that thread
// runs its function without locking again (the engine thread is locked to its OS thread: ovThreadMain). Only sound
// channel changes and plain writes run inside (simStateEffect.batch); while it is held no other lock is taken but the
// console's leaf mutex. Host audio timing only: the simulation does the same. Env IKEMEN_SIM_SPEAKER_BATCH=0: one lock
// per channel change, as before.
var simSpeakerThread atomic.Uint64

// simSpeakerBatchMax: a sound batch gives the speaker lock back after this long (the host mixer waits for it, and gave
// silence while a long flush held it)
const simSpeakerBatchMax = 300 * time.Microsecond
var simSpeakerBatch = os.Getenv("IKEMEN_SIM_SPEAKER_BATCH") != "0"


func simStopStateSound(c *Char) {
	x := c.vm().ctx
	// A player's channels are changed only by its own component's sound
	// controllers (cross-component ones keep the barrier), so an empty list
	// with no deferred PlaySnd of this component stays empty at the slot.
	if len(sys.charSoundChannels[c.playerNo]) == 0 && (x == nil || !x.soundPending) {
		return
	}
	stop := func() {
		for _, ch := range c.getOwnChannels(-1) {
			if ch.stopOnChangeState {
				ch.Reset()
				ch.stopOnChangeState = false
			}
		}
	}
	if x != nil && !x.onEngine && simStateDefer && simDeferSound {
		x.commit(stop, simLocalStateSound)
		return
	}
	simOnEngineKind(c, stop, "state_sound")
}

// Sound channels: why an ordered effect is exact.
//
// Every write to sys.charSoundChannels from state code is a PlaySnd, StopSnd,
// stop-on-state-change, SuperPause sound, SndPan or ModifySnd; every read
// (SoundVar, SndPan, ModifySnd) is an engine-lane operation, which first
// flushes all effects of lower or equal rank. Shared operations run in
// (rank, order) order and effects are flushed in the same order before each of
// them, so the channel lists go through the same sequence of changes as when
// these controllers were engine-lane barriers and every reader sees the same
// list. A commit runs with its component on the engine lane, so its console
// warnings stay in place. The only remaining difference is real-time host
// audio: the first pan snapshot (ovAudioTrackSound, refreshed at the end of
// every tick by ovAudioSnapshotPan) reads the owner's x when the effect runs.

// x.commit queues fn as an ordered effect at the controller's (rank, order).
func (x *simStateContext) commit(fn func(), kind int) {
	x.local[kind]++
	x.deferEffect(func() {
		x.onEngine = true
		defer func() { x.onEngine = false }()
		fn()
	})
	x.effects[len(x.effects)-1].kind = int8(kind + 1)
	x.effects[len(x.effects)-1].batch = kind == simLocalPlaySnd || kind == simLocalStopSnd || kind == simLocalStateSound
}

// simSoundEffect runs fn (a sound-channel change of crun) as an ordered
// effect. It returns false when fn should run now: outside the pool or on the
// engine lane. Changes to another component's channels keep the barrier.
func simSoundEffect(c, crun *Char, fn func()) bool {
	x := c.vm().ctx
	if x == nil || x.onEngine {
		return false
	}
	if simStateDefer && crun.simCtx == c.simCtx {
		x.commit(fn, simLocalStopSnd)
		return true
	}
	simOnEngineKind(c, fn, "controller")
	return true
}

// simPlaySnd: PlaySnd's parameters are already evaluated in the component;
// the channel request/play follows as an ordered effect. Facing and the
// warning prefix are read now, as the engine-lane controller read them.
// Channel inheritance (parent/root lookups) keeps the barrier.
func simPlaySnd(c, crun *Char, params *PlaySndParams) bool {
	x := c.vm().ctx
	if x == nil || x.onEngine {
		return false
	}
	if simStateDefer && crun.simCtx == c.simCtx && crun.inheritChannels == 0 {
		facing, warn := crun.facing, crun.warn()
		x.soundPending = true
		x.commit(func() { crun.playSoundAt(params, facing, warn) }, simLocalPlaySnd)
		return true
	}
	simOnEngineKind(c, func() { crun.playSound(params) }, "controller")
	return true
}

// simCornerPushComponent returns the component whose characters the
// cornerpush scan of c must examine, or -1 for the full stock scan.
//
// Exact: the scan only uses a getter with getter.ghv.playerid == c.id. The
// dependency graph joins every character whose ghv.playerid names c (and c's
// targets) into c's component at the start of the pass; during the pass that
// field is written only by hit detection (after the join), by a reset to -1
// (new helpers) and by GetHitVarSet. A getter of another component therefore
// fails the test and is skipped without reading its state, so the scan reads
// only this component and needs no barrier. The one exception would be a
// GetHitVarSet (engine lane) that stores c's ID as the attacker of a
// character outside c's component in the same tick. Globals the scan reads
// (supertime, screen edges, camera view) are written in the pass only by
// SuperPause and CameraCtrl, engine-lane controllers whose writes other
// components see from their next shared operation, as before.
func simCornerPushComponent(c *Char) int {
	x := c.vm().ctx
	if x == nil || x.onEngine || !simStateDefer || !simDeferCorner {
		return -1
	}
	x.local[simLocalCornerPush]++
	return x.pool.comp[c.playerNo]
}

// simLifeSet: Char.lifeSet of c inside its own component (before, every LifeSet/LifeAdd - and every
// TargetLifeAdd landing in the component - took the serial lane, reason "shared"). It is Char.lifeSet line for line
// with its two global writes, sys.winType and sys.lastHitter, made ordered effects at the call's (rank, order).
// Exact: everything else it reads and writes is c's own (life, redLife, ghv, ss, immortal) or fixed during the pass
// (sys.intro for roundNoDamage, the fight screen's red life flag); nothing reads sys.winType or sys.lastHitter during
// the pass in FFA - win() is false in FFA, so WinPerfect/WinClutch never reach winType, and lastHitter is read only
// by the motif after the match - and the effects write them in the same (rank, order) sequence as the serial lane
// did, so the last write wins the same. Team.LifeShare is off whenever the pool runs (simStateEnabled). Only a char
// of the caller's own component, outside an exclusive turn; else false (the serial lane, as before). Env
// IKEMEN_SIM_LIFE=0: the serial lane always.
func simLifeSet(c *Char, x *simStateContext, life int32) bool {
	if !simStateDefer || !simDeferLife || c.simCtx != x || x.exclusive {
		return false
	}
	x.local[simLocalLifeSet]++
	if c.alive() && sys.roundNoDamage() {
		return true
	}
	c.life = Clamp(life, 0, c.lifeMax)
	if c.life == 0 {
		// Check win type
		if c.isPlayerType() && c.teamside != -1 {
			if c.alive() && c.helperIndex == 0 {
				var wt WinType
				if c.ss.moveType != MT_H {
					if c.playerNo == c.ss.sb.playerNo {
						wt = WT_Suicide
					} else if c.playerNo&1 == c.ss.sb.playerNo&1 {
						wt = WT_Teammate
					} else {
						wt = -1 // (no write)
					}
				} else if c.playerNo == c.ghv.playerno {
					wt = WT_Suicide
				} else if c.ghv.playerno >= 0 && c.playerNo&1 == c.ghv.playerno&1 {
					wt = WT_Teammate
				} else if c.ghv.guardko {
					wt = WT_Cheese
				} else if c.ghv.attr&int32(AT_AH) != 0 {
					wt = WT_Hyper
				} else if c.ghv.attr&int32(AT_AS) != 0 {
					wt = WT_Special
				} else if c.ghv.attr&int32(AT_AT) != 0 {
					wt = WT_Throw
				} else {
					wt = WT_Normal
				}
				if wt >= 0 {
					side := ^c.playerNo & 1
					x.deferEffect(func() { sys.winType[side] = wt })
					x.effects[len(x.effects)-1].batch = true
				}
			}
		} else if c.immortal { // in mugen even non-player helpers can die
			c.life = 1
		}
		c.redLife = 0
	}
	if c.teamside != c.ghv.playerno&1 && c.teamside != -1 && c.ghv.playerno < MaxSimul*2 { // attacker and receiver from opposite teams
		side, pn := ^c.playerNo&1, c.ghv.playerno
		x.deferEffect(func() { sys.lastHitter[side] = pn })
		x.effects[len(x.effects)-1].batch = true
	}
	// Disable red life. Placing this here makes it never lag behind life
	if !c.redLifeEnabled() {
		c.redLife = c.life
	}
	return true
}

var simDeferLife = os.Getenv("IKEMEN_SIM_LIFE") != "0"

var simProjLocal = os.Getenv("IKEMEN_SIM_PROJ") != "0"

// simEnvShake: the EnvShake controller inside the component (before: the serial lane, reason
// "controller", ~0.3-1 a pass in a 101-fighter fight). envShake.Run clears sys.envShake, sets it from the parameters,
// then (FFA) ovEnvShake turns it into a camera event for Godot and clears it again: what it leaves depends on the
// parameters only. With every parameter local (simParamsLocal: constants and the char's own state - no
// EnvShakeVar, no redirect) they are evaluated here, where the serial lane evaluated them, into a copy, and
// sys.envShake gets that copy, the event and the clear as an ordered effect at the controller's (rank, order).
// sys.envShake is written in the pass only by EnvShake (now this effect) and FallEnvShake (serial lane, which flushes
// lower ranks first), so the writes keep their order; it is read in the pass only by EnvShakeVar, which parks this
// component first while its own shake waits (simEnvShakeSync) - so it reads what the serial lane left - and other
// components see the write from their next shared operation, as before. Env IKEMEN_SIM_SHAKE=0: the serial lane.
func simEnvShake(sc envShake, c *Char) {
	x := c.vm().ctx
	var es EnvShake
	es.clear()
	StateControllerBase(sc).run(c, func(paramID byte, exp []BytecodeExp) bool {
		switch paramID { // (envShake.Run's, into es)
		case envShake_time:
			es.time = exp[0].evalI(c)
		case envShake_ampl:
			es.ampl = float32(int32(float32(exp[0].evalI(c)) * c.localscl))
		case envShake_freq:
			es.freq = Max(0, exp[0].evalF(c))
		case envShake_phase:
			es.phase = Clamp(exp[0].evalF(c), -180, 180)
		case envShake_mul:
			es.mul = exp[0].evalF(c)
		case envShake_dir:
			es.dir = exp[0].evalF(c)
		case envShake_diradd:
			es.diradd = exp[0].evalF(c)
		case envShake_decay:
			es.decay = exp[0].evalF(c)
		}
		return true
	})
	es.setDefaultPhase()
	es.restart()
	x.local[simLocalEnvShake]++
	x.shakePending = true
	x.deferEffect(func() {
		sys.envShake = es
		ovEnvShake(c)
	})
}

var simDeferShake = os.Getenv("IKEMEN_SIM_SHAKE") != "0"

// simEnvShakeSync: EnvShakeVar read while this component's own EnvShake waits as an effect - park first, so the
// effect has run (bytecode.go run_ex2)
func simEnvShakeSync(vm *simVM) {
	if x := vm.ctx; x != nil && x.shakePending && !x.onEngine {
		x.rendezvous(func() {}, false, "env_shake_pending")
	}
}

// simParamsLocal: every parameter expression of a controller is built only from simHelperExpLocal's opcodes
// (constants, arithmetic, the char's own state, the component's RNG)
func simParamsLocal(scb StateControllerBase) bool {
	for i := 0; i < len(scb); {
		n := int(scb[i+1])
		i += 2
		for m := 0; m < n; m++ {
			l := int(*(*int32)(unsafe.Pointer(&scb[i])))
			i += 4
			if !simHelperExpLocal((*(*BytecodeExp)(unsafe.Pointer(&scb)))[i : i+l]) {
				return false
			}
			i += l
		}
	}
	return true
}

// simCallSite: (env IKEMEN_SIM_STATS) a serial turn's reason with the two calls above simOwn - the redirect and its user
func simCallSite(reason string) string {
	for skip := 2; skip <= 5; skip++ {
		if pc, file, line, ok := runtime.Caller(skip); ok {
			name := "?"
			if f := runtime.FuncForPC(pc); f != nil {
				name = filepath.Base(f.Name())
			}
			reason += fmt.Sprintf(" < %s %s:%d", name, filepath.Base(file), line)
		}
	}
	return reason
}

// Dev: env IKEMEN_SIM_LABELS=1 - each char of the pass runs under pprof labels: body alive|dead (its fighter's), kind
// root|helper; a CPU profile (IKEMEN_RENDER_PPROF) then splits the pass by them (go tool pprof -tags)
var simLabels = os.Getenv("IKEMEN_SIM_LABELS") != ""

// (IKEMEN_SIM_LABELS=2: the fighter and the state too)
var simLabelsFull = os.Getenv("IKEMEN_SIM_LABELS") == "2"

var simLabelSets = [4]pprof.LabelSet{pprof.Labels("body", "alive", "kind", "root"), pprof.Labels("body", "alive", "kind", "helper"),
	pprof.Labels("body", "dead", "kind", "root"), pprof.Labels("body", "dead", "kind", "helper")}

func simLabelOf(c *Char) pprof.LabelSet {
	if simLabelsFull {
		body, kind := "alive", "root"
		if r := ovLive(c.playerNo); r != nil && !r.alive() {
			body = "dead"
		}
		if c.helperIndex != 0 {
			kind = "helper"
		}
		name := "?"
		if r := ovLive(c.playerNo); r != nil {
			name = r.name
		}
		return pprof.Labels("body", body, "kind", kind, "char", name, "state", strconv.Itoa(int(c.ss.no)))
	}
	i := 0
	if c.helperIndex != 0 {
		i = 1
	}
	if r := ovLive(c.playerNo); r != nil && !r.alive() {
		i += 2
	}
	return simLabelSets[i]
}
