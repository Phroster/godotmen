package main

// State ownership and interaction diagnostics share the same union-find graph.
// State execution needs pointer dependencies; the larger diagnostic graph also
// includes spatial overlap for the collision pass, which remains sequential.
import (
	"math"
	"sync/atomic"
)

type simGraph struct {
	parent                     [MaxPlayerNo]int
	roots, chars               [MaxPlayerNo]int
	groups, maxRoots, maxChars int
	edges                      []simGraphEdge
	noEdges                    bool // (the joins only: edges records nothing - nobody reads it)
}

type simGraphEdge struct {
	A, B int
	Why  string
}

// simGraphSkip: (diagnostic, env IKEMEN_SIM_STATS) an edge kind left out of a graph built for the stats only
var simGraphSkip string

func (g *simGraph) edge(a, b int, why string) {
	if simGraphSkip != "" && simGraphSkips(why) {
		return
	}
	if a < 0 || b < 0 || a >= MaxPlayerNo || b >= MaxPlayerNo || g.find(a) == g.find(b) {
		return
	}
	if !g.noEdges {
		g.edges = append(g.edges, simGraphEdge{a, b, why})
	}
	g.join(a, b)
}

// simGraphSkips: the edge kind is left out of the stats' graph (simGraphSkip: one kind, or a group of them)
func simGraphSkips(why string) bool {
	switch simGraphSkip {
	case why:
		return true
	case "dead links":
		return why == "last attacker (dead)" || why == "focus (dead)"
	case "last attacker*":
		return why == "last attacker" || why == "last attacker (dead)"
	case "focus*":
		return why == "focus" || why == "focus (dead)"
	}
	return false
}

func (g *simGraph) find(n int) int {
	for g.parent[n] != n {
		g.parent[n] = g.parent[g.parent[n]]
		n = g.parent[n]
	}
	return n
}

func (g *simGraph) join(a, b int) {
	if a < 0 || b < 0 || a >= MaxPlayerNo || b >= MaxPlayerNo {
		return
	}
	a, b = g.find(a), g.find(b)
	if a > b {
		a, b = b, a
	}
	g.parent[b] = a
}

// simPruneEdges: a fighter's last attacker stops joining its component while that attacker's cornerpush scan cannot
// run (simCornerPushLive) - the scan is the one reader of that link that relies on the graph; state code reaches the
// attacker only through a redirect (simOwn's exclusive turn: exact either way, and rare). The link outlives its use:
// it is kept until the next hit, so a corpse - never hit again - joined its killer for good, and with its focus
// (kept: its P2Name/p2/enemy triggers read it every tick) each corpse bridged two components until a 101-fighter
// fight ran as one component on one worker - the whole pass serial (IKEMEN_SIM_STATS). ovConfigure
// key sim_prune_edges, env IKEMEN_SIM_PRUNE=0/1 (default on). Like any change of the components it gives other RNG
// streams, so other FFA fights; worker counts still agree.
var simPruneEdges = true

// simGraphPrimedFocus: the state graph's focus edges are the primed focus (simStates.focus) - see simBuildGraph.
// ovConfigure key sim_graph_primed_focus, env IKEMEN_SIM_FOCUS=0/1 (default on). Other components, so other RNG
// streams and other FFA fights; worker counts still agree.
var simGraphPrimedFocus = true

// simIdle: c runs no state code, physics or update this pass - its actionRun returns at once. Such a char reads
// nothing in the pass, so it needs no links of its own and primes no focus. Nothing idles a char in this build
// (disabled chars are skipped by the pass already); a host feature that parks chars answers here.
func simIdle(c *Char) bool { return false }

// simCornerPushLive: the char with this ID may run its cornerpush scan this pass (checkCornerPush returns at once
// while its cornerpush_veloff is 0; hit detection sets it after the pass, and in the pass it only decays to 0)
func simCornerPushLive(id int32) bool {
	a := sys.charList.idMap[id]
	return a != nil && a.mhv.cornerpush_veloff != 0
}

func simInteractionGraph() simGraph {
	return simBuildGraph(true)
}

// Collision/push execute after the state completion barrier. Spatial overlap
// alone is therefore not a dependency of actionRun: focus, ownership, targets,
// custom states and binds are. Keep the envelope graph for diagnostics.
// (Every tick: its edge list is kept only for the stats, IKEMEN_SIM_STATS - the components are the same either way.)
func simStateDependencyGraph() simGraph {
	return simBuildGraphOf(false, simStats == nil)
}

func simBuildGraph(collision bool) simGraph { return simBuildGraphOf(collision, false) }

// simGraphIDOwner: simBuildGraph's id -> player map, refilled for each graph (not allocated and grown every tick)
var simGraphIDOwner = map[int32]int{}

func simBuildGraphOf(collision, noEdges bool) simGraph {
	g := simGraph{noEdges: noEdges}
	for i := range g.parent {
		g.parent[i] = i
	}
	idOwner := simGraphIDOwner
	for id := range idOwner {
		delete(idOwner, id)
	}
	for _, c := range sys.charList.runOrder {
		idOwner[c.id] = c.playerNo
	}
	joinID := func(pn int, id int32, why string) {
		if to, ok := idOwner[id]; ok {
			g.edge(pn, to, why)
		}
	}
	for _, c := range sys.charList.runOrder {
		pn := c.playerNo
		if !collision && simIdle(c) {
			// A char that runs nothing this pass (simIdle) reads and writes nothing: its own links go. Kept: its
			// last attacker, whose cornerpush scan reads it; every link a running char has to it is that char's own.
			if simCornerPushLive(c.ghv.playerid) {
				joinID(pn, c.ghv.playerid, "last attacker (dead)")
			}
			continue
		}
		g.edge(pn, c.ss.sb.playerNo, "custom state") // shared custom-state owner/persistent counters
		joinID(pn, c.parentId, "parent")
		// (simCornerPushComponent: the scan of c reads every getter whose ghv.playerid is c's, so they share it)
		if collision || !simPruneEdges || simCornerPushLive(c.ghv.playerid) {
			if c.alive() {
				joinID(pn, c.ghv.playerid, "last attacker")
			} else {
				joinID(pn, c.ghv.playerid, "last attacker (dead)")
			}
		}
		// shouldFaceP2 consults the backup only when current focus is absent.
		// Intro-era backup IDs otherwise connect unrelated duels forever.
		if collision || simStates.focus[pn] == nil {
			joinID(pn, c.p2EnemyBackup, "p2 backup")
		}
		g.edge(pn, c.animPN, "animation owner")
		for _, entry := range c.ghv.targetedBy {
			joinID(pn, entry[0], "targeted by")
		}
		if c.bindTime != 0 {
			joinID(pn, c.bindToId, "bind")
		}
		for _, id := range c.targets {
			joinID(pn, id, "target")
		}
		for _, id := range c.children {
			joinID(pn, id, "child")
		}
	}
	// simGraphPrimedFocus: the focus edge is the focus the pass reads. Every in-pass ovFocus/p2/enemy/enemyNear of a char
	// of player pn returns simStates.focus[pn] (primed by simRunStates before this graph, through ovFocus itself), so
	// that pointer - and nothing else of the host's focus - is a dependency of pn's state code. The host's slot focus
	// (the else branch) also names a corpse, which ovFocus passes over for the nearest live enemy: the corpse then
	// bridged its killer's duel with its own focus's (one hub per kill), and the live focus that p2 actually returned
	// lay outside the component - an exclusive "foreign" turn at every p2 read.
	// (simIdle: which players have a char that runs this pass - only those read their focus)
	var runs [MaxPlayerNo]bool
	anyIdle := false
	if !collision {
		for _, c := range sys.charList.runOrder {
			if c.scf(SCF_disabled) || c.csf(CSF_destroy) {
				continue
			}
			if simIdle(c) {
				anyIdle = true
			} else {
				runs[c.playerNo] = true
			}
		}
	}
	if !collision && simGraphPrimedFocus {
		for pn := range simStates.focus {
			f := simStates.focus[pn]
			if f == nil || anyIdle && !runs[pn] {
				continue
			}
			focusWhy := "focus"
			if root := ovLive(pn); root != nil && !root.alive() {
				focusWhy = "focus (dead)"
				if simFocusLog {
					atomic.AddInt64(&simDeadFocusRunning, 1)
				}
			}
			g.edge(pn, f.playerNo, focusWhy)
		}
	}
	// Read focus without priming ovFocus's lazy cache or changing its semantics.
	for pn := 0; pn < MaxSimul*2 && (collision || !simGraphPrimedFocus); pn++ {
		root := ovLive(pn)
		if root == nil {
			continue
		}
		focusWhy := "focus"
		if !root.alive() {
			focusWhy = "focus (dead)"
		}
		focus := ov.slots[pn].focus
		if ovLive(focus) == nil {
			focus, best := -1, float32(math.MaxFloat32)
			for en := 0; en < MaxSimul*2; en++ {
				e := ovLive(en)
				if e == nil || !ovIsEnemy(root, e) {
					continue
				}
				dx := e.pos[0]*e.localscl - root.pos[0]*root.localscl
				dz := (e.pos[2]*e.localscl - root.pos[2]*root.localscl) * 4
				if d := float32(math.Hypot(float64(dx), float64(dz))); d < best {
					focus, best = en, d
				}
			}
			g.edge(pn, focus, focusWhy)
		} else {
			g.edge(pn, focus, focusWhy)
		}
	}
	type box struct {
		pn           int
		x, z, rx, rz float32
	}
	var boxes []box
	if collision {
		boxes = make([]box, 0, len(sys.charList.runOrder))
		for _, c := range sys.charList.runOrder {
			if c.csf(CSF_destroy) || c.scf(SCF_disabled) {
				continue
			}
			rx := Max(c.sizeWidth[0], c.sizeWidth[1]) * c.localscl
			for clsn := int32(1); clsn <= 2; clsn++ {
				for _, rect := range c.getClsn(clsn) {
					rx = Max(rx, Abs(rect[0]*c.clsnScale[0]*c.localscl), Abs(rect[2]*c.clsnScale[0]*c.localscl))
				}
			}
			// Current boxes plus two ticks of current speed and 32px/8px margin.
			// A diagnosis, not a proof against arbitrary PosSet/new hitbox code.
			rx += Abs(c.vel[0]*c.localscl)*2 + 32*ovWPP()
			rz := Max(c.sizeDepth[0], c.sizeDepth[1], c.hitdef.attack_depth[0], c.hitdef.attack_depth[1]) * c.localscl
			rz += Abs(c.vel[2]*c.localscl)*2 + 8*ovWPP()
			boxes = append(boxes, box{c.playerNo, c.pos[0] * c.localscl, c.pos[2] * c.localscl, rx, rz})
		}
	}
	for pn := range sys.projs {
		for _, p := range sys.projs[pn] {
			if p == nil || !p.isActive() {
				continue
			}
			joinID(pn, p.ownerId, "projectile owner")
			if collision {
				rx := Abs(p.velocity[0]*p.localscl)*2 + 64*ovWPP()
				rz := Max(p.hitdef.attack_depth[0], p.hitdef.attack_depth[1])*p.localscl + 8*ovWPP()
				boxes = append(boxes, box{pn, p.pos[0] * p.localscl, p.pos[2] * p.localscl, rx, rz})
			}
		}
		for _, e := range sys.explods[pn] {
			if e != nil && e.id >= 0 {
				joinID(pn, e.ownerId, "explod owner")
				joinID(pn, e.bindId, "explod bind")
			}
		}
	}
	if collision {
		for i, a := range boxes {
			for _, b := range boxes[i+1:] {
				if a.pn != b.pn && Abs(a.x-b.x) <= a.rx+b.rx && Abs(a.z-b.z) <= a.rz+b.rz {
					g.edge(a.pn, b.pn, "collision envelope")
				}
			}
		}
	}
	// Players with only idle chars, left in components of their own, run as one: nothing in them runs, so one
	// dispatch instead of one per corpse.
	if anyIdle {
		var compRuns [MaxPlayerNo]bool
		for pn := range runs {
			if runs[pn] {
				compRuns[g.find(pn)] = true
			}
		}
		anchor := -1
		for _, c := range sys.charList.runOrder {
			pn := c.playerNo
			if runs[pn] || compRuns[g.find(pn)] || c.scf(SCF_disabled) {
				continue
			}
			if anchor < 0 {
				anchor = pn
			} else {
				g.join(anchor, pn)
			}
		}
	}
	for pn := 0; pn < MaxSimul*2; pn++ {
		if ovLive(pn) != nil {
			g.roots[g.find(pn)]++
		}
	}
	for _, c := range sys.charList.runOrder {
		if !c.csf(CSF_destroy) && !c.scf(SCF_disabled) {
			g.chars[g.find(c.playerNo)]++
		}
	}
	for i, roots := range g.roots {
		if roots > 0 {
			g.groups++
			g.maxRoots = Max(g.maxRoots, roots)
			g.maxChars = Max(g.maxChars, g.chars[i])
		}
	}
	return g
}
