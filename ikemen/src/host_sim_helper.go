package main

import (
	"fmt"
	"os"
	"unsafe"
)

// Deferred Helper creation in the FFA state pool.
//
// Stock order of the Helper controller: newHelper (slot, ID, charList, the
// parent's children), the parameters (evaluated with the new helper already
// existing), then helperInit (position from the creator, palette, the first
// ChangeState, actionPrepare). On the serial lane that is one barrier per
// helper. Deferred, the component evaluates what only it can see and the
// global part follows as an ordered effect at the controller's (rank, order):
//
//   - The parameters are evaluated in the component, before the helper exists.
//     Exact only when they cannot observe it: every expression must be built
//     from the opcodes in simHelperExpLocal (literals, arithmetic, the
//     controller owner's own vars/time/pos/vel/life...). Anything else (a
//     redirect, NumHelper, IsHelper, PlayerID, ...) keeps the engine lane.
//   - Creation can fail only at the helper limit; the slot table of the
//     creator's player is changed in this pass only by its own component
//     (destroy runs after the pass), so the outcome is predicted exactly. A
//     predicted failure keeps the stock code on the engine lane.
//   - Everything newHelper/helperInit read from the creator (and its focus for
//     postype p2) is captured at the controller (simHelperSrc); the effect then
//     replays the stock code with those values. Globals (IDs, idMap,
//     charList, camera, round state) are read when the effect runs, at the same
//     place in the (rank, order) sequence as the barrier.
//   - Until the effect has run, every read that could see the helper parks
//     the component first (simHelperSync): NumHelper, helper() and
//     helperindex redirects, IsHelper, children walks (DestroySelf recursive,
//     clsn proxies, IgnoreDarkenTime), a second Helper of the component. IDs
//     are already safe: an ID above the tick's high-water mark is a
//     "missing_id" barrier (simPlayerID) and LastPlayerID is engine lane.
//   - ownpal helpers (palette copy and remap of the creator) keep a barrier,
//     with the parameters already evaluated.
//
// Switch: simDeferHelper (ovConfigure key sim_state_defer_helper, env
// IKEMEN_SIM_STATE_DEFER_HELPER=0/1); off = perf2's barrier exactly. The
// variable starts off (the standalone build), and simConfigure turns it ON
// for the overlay: exact (1/4/6-worker digests), and faster
// once the giant interaction component was gone (simPruneEdges; with a
// deferred helper's ovBloodPin fixed, perf-lane-c 943297e).
var simDeferHelper = false

// simHelperMiss counts deferred creations that found no slot (see simHelperCreate).
var simHelperMiss uint64

// simHelperSync parks the component when one of its helper creations is still
// an ordered effect. Every engine-lane operation flushes effects of lower or
// equal rank first, so the helper exists afterwards.
//
// Per player: every such read looks at c's own player only - helperTrigger, getHelperChainIndex and
// IsHelper filter sys.charList.creationOrder by c.playerNo, NumHelper walks sys.chars[c.playerNo], and DestroySelf,
// clsn proxies and IgnoreDarkenTime walk c.children, which are always helpers of c's player (newHelper is the only
// writer and a helper's player is its creator's). A pending creation for another player of the component is
// invisible to all of them, so only a pending helper of c's own player parks c. (IKEMEN_SIM_HELPER_MULTI=0: any
// pending helper of the component parks, as before.)
func simHelperSync(c *Char) {
	if c == nil {
		return
	}
	x := c.vm().ctx
	if x == nil || x.onEngine || len(x.helperPending) == 0 {
		return
	}
	if simHelperMulti && !x.helperPendingFor(c.playerNo) {
		return
	}
	reason := "helper_pending"
	if simStats != nil {
		reason = simCallSite(reason)
	}
	x.rendezvous(func() {}, false, reason)
}

// simHelperMulti: several deferred Helper creations may wait in one component (per-player sync and slot
// prediction); off = a second Helper of the component takes the barrier. Env IKEMEN_SIM_HELPER_MULTI=0/1.
var simHelperMulti = os.Getenv("IKEMEN_SIM_HELPER_MULTI") != "0"

// simNumHelperLocal: NumHelper counts pending creations instead of parking (simNumHelperPending); env
// IKEMEN_SIM_NUMHELPER=0 parks.
var simNumHelperLocal = os.Getenv("IKEMEN_SIM_NUMHELPER") != "0"

// simHelperTriggerLocal: helper(id) redirects look at the existing helpers first and park only when a pending helper
// of the player could be the answer (Char.helperTrigger: pending creations are appended after every existing helper,
// so they matter only when the existing ones do not reach the index; then a pending one matches by its helper ID).
// Env IKEMEN_SIM_HELPERTRIGGER=0: park first.
var simHelperTriggerLocal = os.Getenv("IKEMEN_SIM_HELPERTRIGGER") != "0"

// simHelperPendingMatch: a pending creation of c's player whose helper ID matches id (id <= 0: any)
func simHelperPendingMatch(c *Char, id int32) bool {
	if c == nil {
		return false
	}
	x := c.vm().ctx
	if x == nil || x.onEngine {
		return false
	}
	for _, p := range x.helperPending {
		if p.pn == c.playerNo && (id <= 0 || p.id == id) {
			return true
		}
	}
	return false
}

// simHelperLocalMore: ownpal helpers and a creation that fails at the helper limit stay in the component too;
// env IKEMEN_SIM_HELPER_OWNPAL=0 keeps their barriers.
var simHelperLocalMore = os.Getenv("IKEMEN_SIM_HELPER_OWNPAL") != "0"

// simPendingHelper: a deferred creation's player and the helper ID it will have (its id parameter; a negative one
// becomes 0 in helperInit)
type simPendingHelper struct {
	pn int
	id int32
}

func (x *simStateContext) helperPendingFor(pn int) bool {
	for _, p := range x.helperPending {
		if p.pn == pn {
			return true
		}
	}
	return false
}

// simNumHelperPending: NumHelper(hid) of c without parking for a pending helper of c's player. The
// creation is certain (simHelper predicted its slot) and NumHelper counts every helper of the player not marked
// CSF_destroy: a reused slot held a destroyed helper (destroy sets CSF_destroy and helperIndex -1), which was not
// counted, and the new helper is counted when its ID matches (id <= 0 matches any). So the count after the effect is
// the count now plus the matching pending creations. Returns that addend; with IKEMEN_SIM_HELPER_MULTI=0 it parks as
// before (simHelperSync) and returns 0.
func simNumHelperPending(c *Char, hid BytecodeValue) int32 {
	if c == nil {
		return 0
	}
	x := c.vm().ctx
	if x == nil || x.onEngine || len(x.helperPending) == 0 {
		return 0
	}
	if !simHelperMulti || !simNumHelperLocal {
		simHelperSync(c)
		return 0
	}
	if hid.IsUndefined() {
		return 0
	}
	id, n := hid.ToI(), int32(0)
	for _, p := range x.helperPending {
		if p.pn == c.playerNo && (id <= 0 || p.id == id) {
			n++
		}
	}
	return n
}

type simHelperSrc struct {
	pn                     int
	name                   string
	id                     int32
	origin                 int32
	controller             int
	teamside               int
	localcoord, localscl   float32
	size                   CharSize
	lifeMax, powerMax      int32
	dizzyMax, guardMax     int32
	hLocalcoord, hLocalscl float32
	pos                    [3]float32
	facing                 float32
	palfx                  *PalFX
	ignoreDarkenTime       int32
	ownpal                 bool // palfx is the helper's own (ownpal), else the creator's shared one
	nest                   int32
	workingChar            *Char
	workingState           *StateBytecode
}

// simHelper handles the Helper controller inside the state pool. It returns
// false outside the pool or on the engine lane (stock code runs).
func simHelper(sc helper, c, crun *Char) bool {
	x := c.vm().ctx
	if x == nil || x.onEngine {
		return false
	}
	why := "helper"
	engine := func() {
		simOnEngineKind(c, func() { sc.runOn(c, crun) }, why)
	}
	if !simStateDefer || !simDeferHelper {
		engine()
		return true
	}
	if crun.simCtx != c.simCtx || !simHelperMulti && len(x.helperPending) > 0 || !simHelperParamsLocal(sc) {
		if simStats != nil {
			switch {
			case crun.simCtx != c.simCtx:
				why = "helper:foreign"
			case !simHelperMulti && len(x.helperPending) > 0:
				why = "helper:second"
			default:
				why = "helper:params " + simHelperParamsWhy(sc)
			}
		}
		engine()
		return true
	}
	// Stock newHelper: the first slot with helperIndex < 0, else append below
	// the helper limit. Only this component changes this player's slots: its
	// own pending creations (effect order = controller order) take the free
	// slots first, then append one each.
	pn := crun.playerNo
	pend := 0
	for _, p := range x.helperPending {
		if p.pn == pn {
			pend++
		}
	}
	free := 0
	for hidx := 1; hidx < len(sys.chars[pn]); hidx++ {
		if sys.chars[pn][hidx].helperIndex < 0 {
			free++
		}
	}
	if pend >= free && int64(len(sys.chars[pn]))+int64(pend-free) > int64(sys.cfg.Config.HelperMax) {
		if simHelperLocalMore {
			// Stock: newHelper finds no slot, warns and returns - the parameters are never evaluated. The warning is
			// all it does (an ordered effect, its text read here, where the barrier's newHelper read it).
			root := sys.chars[pn][0]
			crun.simConsole(root.warn() + fmt.Sprintf("Reached limit of %v helpers. Helper creation skipped", sys.cfg.Config.HelperMax))
			x.local[simLocalHelper]++
			return true
		}
		if simStats != nil {
			why = "helper:full"
		}
		engine()
		return true
	}

	// The stock parameter loop, recorded as operations on the new helper.
	redirscale := c.localscl / crun.localscl
	pt := PT_P1
	var f, st int32 = 1, 0
	var pos [3]float32
	rp := [2]int32{-1, 0}
	ownpal := false
	var hid int32 // (the helper ID: newHelper's 0 unless the id parameter sets it)
	var ops []func(h *Char)
	op := func(fn func(h *Char)) { ops = append(ops, fn) }
	StateControllerBase(sc).run(c, func(paramID byte, exp []BytecodeExp) bool {
		switch paramID {
		case helper_helpertype:
			v := exp[0].evalI(c)
			op(func(h *Char) { h.helperType = v })
		case helper_name:
			v := exp[0].evalS()
			op(func(h *Char) { h.name = v })
		case helper_clsnproxy:
			v := exp[0].evalB(c)
			op(func(h *Char) { h.isclsnproxy = v })
		case helper_postype:
			pt = PosType(exp[0].evalI(c))
		case helper_ownpal:
			v := exp[0].evalB(c)
			ownpal = v
			op(func(h *Char) { h.ownpal = v })
		case helper_size_xscale:
			v := exp[0].evalF(c)
			op(func(h *Char) { h.size.xscale = v })
		case helper_size_yscale:
			v := exp[0].evalF(c)
			op(func(h *Char) { h.size.yscale = v })
		case helper_size_ground_back:
			v := exp[0].evalF(c) * -1
			op(func(h *Char) { h.size.standbox[0] = v })
		case helper_size_ground_front:
			v := exp[0].evalF(c)
			op(func(h *Char) { h.size.standbox[2] = v })
		case helper_size_air_back:
			v := exp[0].evalF(c) * -1
			op(func(h *Char) { h.size.airbox[0] = v })
		case helper_size_air_front:
			v := exp[0].evalF(c)
			op(func(h *Char) { h.size.airbox[2] = v })
		case helper_size_height:
			v := exp[0].evalF(c) * -1
			op(func(h *Char) { h.size.standbox[1] = v })
		case helper_size_proj_doscale:
			v := exp[0].evalI(c)
			op(func(h *Char) { h.size.proj.doscale = v })
		case helper_size_head_pos:
			v0 := exp[0].evalF(c)
			op(func(h *Char) { h.size.head.pos[0] = v0 })
			if len(exp) > 1 {
				v1 := exp[1].evalF(c)
				op(func(h *Char) { h.size.head.pos[1] = v1 })
			}
		case helper_size_mid_pos:
			v0 := exp[0].evalF(c)
			op(func(h *Char) { h.size.mid.pos[0] = v0 })
			if len(exp) > 1 {
				v1 := exp[1].evalF(c)
				op(func(h *Char) { h.size.mid.pos[1] = v1 })
			}
		case helper_size_shadowoffset:
			v := exp[0].evalF(c)
			op(func(h *Char) { h.size.shadowoffset = v })
		case helper_size_depth:
			v0 := exp[0].evalF(c)
			op(func(h *Char) { h.size.depth[0] = v0 })
			if len(exp) > 1 {
				v1 := exp[1].evalF(c)
				op(func(h *Char) { h.size.depth[1] = v1 })
			}
		case helper_size_weight:
			v := exp[0].evalI(c)
			op(func(h *Char) { h.size.weight = v })
		case helper_size_pushfactor:
			v := exp[0].evalF(c)
			op(func(h *Char) { h.size.pushfactor = v })
		case helper_stateno:
			st = exp[0].evalI(c)
		case helper_keyctrl:
			for _, e := range exp {
				m := e.run(c).ToI()
				op(func(h *Char) {
					if m > 0 && m <= int32(len(h.keyctrl)) {
						h.keyctrl[m-1] = true
					}
				})
			}
		case helper_id:
			v := exp[0].evalI(c)
			hid = v
			op(func(h *Char) { h.helperId = v })
		case helper_pos:
			pos[0] = exp[0].evalF(c) * redirscale
			if len(exp) > 1 {
				pos[1] = exp[1].evalF(c) * redirscale
				if len(exp) > 2 {
					pos[2] = exp[2].evalF(c) * redirscale
				}
			}
		case helper_facing:
			f = exp[0].evalI(c)
		case helper_pausemovetime:
			v := exp[0].evalI(c)
			op(func(h *Char) { h.pauseMovetime = v })
		case helper_supermovetime:
			v := exp[0].evalI(c)
			op(func(h *Char) { h.superMovetime = v })
		case helper_remappal:
			rp[0] = exp[0].evalI(c)
			if len(exp) > 1 {
				rp[1] = exp[1].evalI(c)
			}
		case helper_extendsmap:
			if exp[0].evalB(c) {
				m := make(map[string]float32, len(crun.mapArray))
				for key, value := range crun.mapArray {
					m[key] = value
				}
				op(func(h *Char) {
					for key, value := range m {
						h.mapArray[key] = value
					}
				})
			}
		case helper_inheritjuggle:
			v := exp[0].evalI(c)
			op(func(h *Char) { h.inheritJuggle = v })
		case helper_inheritchannels:
			v := exp[0].evalI(c)
			op(func(h *Char) { h.inheritChannels = v })
		case helper_immortal:
			v := exp[0].evalB(c)
			op(func(h *Char) { h.immortal = v })
		case helper_kovelocity:
			v := exp[0].evalB(c)
			op(func(h *Char) { h.kovelocity = v })
		case helper_preserve:
			v := exp[0].evalB(c)
			op(func(h *Char) { h.preserve = v })
		case helper_ownclsnscale:
			v := exp[0].evalB(c)
			op(func(h *Char) { h.ownclsnscale = v })
		case helper_standby:
			v := exp[0].evalB(c)
			op(func(h *Char) {
				if v {
					h.setSCF(SCF_standby)
				} else {
					h.unsetSCF(SCF_standby)
				}
			})
		case helper_ownprojectile:
			v := exp[0].evalB(c)
			op(func(h *Char) { h.ownProjectile = v })
		case helper_map:
			key := exp[0].evalS()
			v := exp[1].evalF(c)
			op(func(h *Char) { h.mapArray[key] = v })
		}
		return true
	})

	if ownpal && !simHelperLocalMore {
		// helperInit copies and remaps the creator's palette: stock code on
		// the engine lane, parameters already evaluated.
		simOnEngineKind(c, func() {
			h := crun.newHelper()
			if h == nil {
				return
			}
			for _, fn := range ops {
				fn(h)
			}
			simHelperScale(h, crun)
			crun.helperInit(h, st, pt, pos, f, rp)
		}, "helper:ownpal")
		return true
	}

	// Values newHelper and helperInit read from the creator, as at the
	// controller (the component is parked right here in the barrier version).
	src := &simHelperSrc{pn: pn, name: crun.name, id: crun.id, origin: ovAttackOrigin(crun),
		controller: crun.controller, teamside: crun.teamside, localcoord: crun.localcoord, localscl: crun.localscl,
		size: crun.size, lifeMax: crun.lifeMax, powerMax: crun.powerMax, dizzyMax: crun.dizzyPointsMax,
		guardMax: crun.guardPointsMax, nest: x.vm.nest, workingChar: x.vm.workingChar, workingState: x.vm.workingState}
	if crun.minus == -2 || crun.minus == -4 {
		src.hLocalscl, src.hLocalcoord = 320/crun.localcoord, crun.localcoord
	} else {
		src.hLocalscl, src.hLocalcoord = crun.localscl, crun.localcoord
	}
	src.facing = 1 // a new helper's facing (Char.init) before helperInit
	src.pos = crun.helperPos(pt, pos, f, &src.facing, src.hLocalscl, false)
	if ownpal {
		// helperInit's own palette: the creator's remap copied, then the
		// RemapPal parameter applied. It reads and writes only the creator's player - its palfx chain (its parents
		// are its player's) and its palette list (RemapPal runs in the component) - so it is made here, where the
		// barrier's helperInit made it, into a palette of the helper's own, which the effect hands to the helper.
		pfx := newPalFX()
		if crun.getPalfx().remap == nil {
			crun.palfx.remap = crun.gi().palettedata.palList.GetPalMap()
		}
		tmp := crun.getPalfx().remap
		pfx.remap = make([]int, len(tmp))
		copy(pfx.remap, tmp)
		crun.forceRemapPal(pfx, rp)
		src.palfx, src.ownpal = pfx, true
	} else {
		src.palfx = crun.getPalfx()
		src.ignoreDarkenTime = crun.ignoreDarkenTime
	}
	// helperInit's MUGEN 1.1 invertblend write goes to the shared palette of
	// the creator's player: done here, where the stock controller does it.
	if gi := &sys.cgi[pn]; gi.mugenver[0] == 1 && gi.mugenver[1] == 1 && gi.ikemenver[0] == 0 && gi.ikemenver[1] == 0 {
		src.palfx.invertblend = -2
	}
	if hid < 0 {
		hid = 0 // (helperInit: "has negative Helper ID")
	}
	x.helperPending = append(x.helperPending, simPendingHelper{pn: pn, id: hid})
	x.commit(func() { simHelperCreate(x, crun, src, ops, st) }, simLocalHelper)
	return true
}

func simHelperScale(h, crun *Char) {
	if crun.minus == -2 || crun.minus == -4 {
		h.localscl = (320 / crun.localcoord)
		h.localcoord = crun.localcoord
	} else {
		h.localscl = crun.localscl
		h.localcoord = crun.localcoord
	}
}

// simHelperCreate replays newHelper, the parameters and helperInit (ownpal
// off) as an ordered effect, with the creator's values from the controller.
func simHelperCreate(x *simStateContext, crun *Char, src *simHelperSrc, ops []func(h *Char), st int32) {
	vm := &x.vm
	nest, wc, ws := vm.nest, vm.workingChar, vm.workingState
	vm.nest, vm.workingChar, vm.workingState = src.nest, src.workingChar, src.workingState
	defer func() { vm.nest, vm.workingChar, vm.workingState = nest, wc, ws }()

	// newHelper
	pn := src.pn
	var h *Char
	hidx := int(1)
	for ; hidx < len(sys.chars[pn]); hidx++ {
		if sys.chars[pn][hidx].helperIndex < 0 {
			h = sys.chars[pn][hidx]
			h.init(pn, hidx)
			break
		}
	}
	if hidx >= len(sys.chars[pn]) {
		if int32(hidx) > sys.cfg.Config.HelperMax {
			// Predicted at the controller; only a cross-component Helper
			// redirected to this player (an exclusive call, never seen in
			// the tested packs) could take the last slot first. Stock failure, counted.
			simHelperMiss++
			root := sys.chars[pn][0]
			root.simConsole(root.warn() + fmt.Sprintf("Reached limit of %v helpers. Helper creation skipped", sys.cfg.Config.HelperMax))
			return
		}
		h = newChar(pn, hidx)
		sys.chars[pn] = append(sys.chars[pn], h)
	}
	h.simCtx = crun.simCtx
	h.name = src.name + "'s helper"
	h.id = sys.newCharId()
	h.ovOriginState = src.origin
	h.helperId = 0
	h.ownpal = false
	h.preserve = false
	h.remapSpr = make(RemapPreset)
	h.parentId = src.id
	h.controller = src.controller
	h.teamside = src.teamside
	h.localcoord = src.localcoord
	h.localscl = src.localscl
	h.size = src.size
	h.life, h.lifeMax = src.lifeMax, src.lifeMax
	h.powerMax = src.powerMax
	h.dizzyPoints, h.dizzyPointsMax = src.dizzyMax, src.dizzyMax
	h.guardPoints, h.guardPointsMax = src.guardMax, src.guardMax
	h.redLife = h.lifeMax
	h.prepareNextRound()
	crun.children = append(crun.children, h.id)
	sys.charList.add(h)

	// parameters
	for _, fn := range ops {
		fn(h)
	}
	h.localscl, h.localcoord = src.hLocalscl, src.hLocalcoord

	// helperInit (ownpal off)
	h.facing = src.facing
	h.setPosX(src.pos[0], true)
	h.setPosY(src.pos[1], true)
	h.setPosZ(src.pos[2], true)
	h.vel = [3]float32{}
	h.palfx = src.palfx
	if !src.ownpal {
		h.ignoreDarkenTime = src.ignoreDarkenTime
	}
	h.changeStateEx(st, pn, 0, 1, "")
	if h.helperId < 0 {
		h.simConsole(h.warn() + fmt.Sprintf("has negative Helper ID"))
		h.helperId = 0
	}
	h.actionPrepare()
}

// simHelperParamsLocal: every parameter expression is built only from
// opcodes that read constants, the VM stack, the component RNG or the
// controller owner's own state (simHelperExpLocal). Such an expression cannot
// observe a helper that does not exist yet.
func simHelperParamsLocal(sc helper) bool {
	scb := StateControllerBase(sc)
	for i := 0; i < len(scb); {
		id := scb[i]
		n := int(scb[i+1])
		i += 2
		for m := 0; m < n; m++ {
			l := int(*(*int32)(unsafe.Pointer(&scb[i])))
			i += 4
			e := (*(*BytecodeExp)(unsafe.Pointer(&scb)))[i : i+l]
			i += l
			switch id {
			case helper_name, helper_redirectid:
				// a string; the redirect is resolved before (getRedirectedChar)
			case helper_map:
				if m == 0 {
					continue // the key string
				}
				fallthrough
			default:
				if !simHelperExpLocal(e) {
					return false
				}
			}
		}
	}
	return true
}

// simHelperParamsWhy: (env IKEMEN_SIM_STATS) the first opcode that keeps a Helper's parameters off the local path
func simHelperParamsWhy(sc helper) string {
	scb := StateControllerBase(sc)
	for i := 0; i < len(scb); {
		id := scb[i]
		n := int(scb[i+1])
		i += 2
		for m := 0; m < n; m++ {
			l := int(*(*int32)(unsafe.Pointer(&scb[i])))
			i += 4
			e := (*(*BytecodeExp)(unsafe.Pointer(&scb)))[i : i+l]
			i += l
			if id == helper_name || id == helper_redirectid || id == helper_map && m == 0 {
				continue
			}
			if op, ok := simHelperExpFirstNonLocal(e); ok {
				return fmt.Sprintf("param%d op%s", id, op)
			}
		}
	}
	return "?"
}

func simHelperExpFirstNonLocal(e BytecodeExp) (string, bool) {
	for i := 0; i < len(e); {
		op := e[i]
		switch op {
		case OC_int8, OC_localvar, OC_jz8, OC_jnz8, OC_jmp8, OC_jsf8, OC_statetype:
			i += 2
			continue
		case OC_int, OC_float, OC_jz, OC_jnz, OC_jmp:
			i += 5
			continue
		case OC_int64:
			i += 9
			continue
		}
		if simHelperExpLocal(e[i : i+1]) {
			i++
			continue
		}
		if (op == OC_ex_ || op == OC_ex2_) && i+1 < len(e) {
			return fmt.Sprintf("ex%d.%d", op, e[i+1]), true
		}
		return fmt.Sprintf("%d", op), true
	}
	return "", false
}

func simHelperExpLocal(e BytecodeExp) bool {
	for i := 0; i < len(e); {
		op := e[i]
		i++
		switch op {
		case OC_int8, OC_localvar, OC_jz8, OC_jnz8, OC_jmp8, OC_jsf8, OC_statetype:
			i++
		case OC_int, OC_float, OC_jz, OC_jnz, OC_jmp:
			i += 4
		case OC_int64:
			i += 8
		case OC_pop, OC_dup, OC_swap, OC_eq, OC_ne, OC_gt, OC_ge, OC_lt, OC_le,
			OC_neg, OC_blnot, OC_bland, OC_blxor, OC_blor, OC_not, OC_and, OC_xor, OC_or,
			OC_add, OC_sub, OC_mul, OC_div, OC_mod, OC_pow, OC_abs, OC_exp, OC_ln, OC_log,
			OC_cos, OC_sin, OC_tan, OC_acos, OC_asin, OC_atan, OC_floor, OC_ceil, OC_ifelse,
			OC_time, OC_animtime, OC_random, OC_pos_x, OC_pos_y, OC_vel_x, OC_vel_y, OC_vel_z,
			OC_facing, OC_anim, OC_alive, OC_life, OC_lifemax, OC_power, OC_powermax,
			OC_var, OC_sysvar, OC_fvar, OC_sysfvar, OC_stateno, OC_prevstateno, OC_rdreset:
		default:
			return false
		}
	}
	return true
}
