package main

// Dev diagnostic, env IKEMEN_SIM_STATS=<file>: every simStatsEvery FFA state passes one JSON line is appended to <file> -
// how many components the passes ran, the largest one (its roots, how many of them are dead, its chars, and what joined
// it: its spanning edges by kind), the serial-lane calls by reason and by controller, and the passes' own wall time.
// Its own file: save/logs/overlay.log is shared by every run in the tree, and a run beside it starts it afresh.
// Unset, nothing here runs (simStats is nil).
import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/metrics"
	"sync"
	"sync/atomic"
	"time"
)

// Dev, env IKEMEN_SIM_FOCUS_LOG=1 (with IKEMEN_SIM_STATS): the in-pass focus reads (ovFocus: p2, enemy, NumEnemy, blood...)
// by whose chars - a live or a dead fighter's, root or helper - and from where, per pass in the stats line
var simFocusLog = os.Getenv("IKEMEN_SIM_FOCUS_LOG") != "" && os.Getenv("IKEMEN_SIM_STATS") != ""
var simFocusMu sync.Mutex
var simFocusReads = map[string]uint64{}

func simStatsFocusRead(c *Char) {
	r := ovLive(c.playerNo)
	if r == nil || r.alive() { // (a live fighter's reads, thousands a pass: only counted)
		atomic.AddInt64(&simFocusAlive, 1)
		return
	}
	key := "dead root"
	if c.helperIndex != 0 {
		key = fmt.Sprintf("dead helper st %d anim %d", c.ss.no, c.animNo)
	}
	simFocusMu.Lock()
	simFocusReads[key]++
	simFocusMu.Unlock()
}

// (dev, IKEMEN_SIM_FOCUS_LOG) the live fighters' focus reads, and the state graph's "focus (dead)" edges by whether the
// dead fighter's body is frozen (only its helpers run) or running
var simFocusAlive, simDeadFocusRunning int64

const simStatsEvery = 120

type simStatsRun struct {
	f                     *os.File
	n                     int
	passNs, passMaxNs     int64
	groups, chars, roots  int
	maxRoots, maxRootsTop int
	topWhy                map[string]int // the largest component at its top: its spanning edges by kind
	topChars              int
	barriers, controllers map[string]uint64
	cmd                   [4]uint64 // the command pool's ticks, parallel, inline and fallback ticks at the last line
	tickUs, publishUs     int64     // whole Ikemen ticks (wake to done) and their draw snapshot publications
	tickN                 int64
	flushKind, flushKindN [simLocalKinds + 1]int64 // effects run (every flush), by kind: time, count
	flushNs, newNs        int64                    // passes: the final flush, the new helpers' first states
	newN                  int
	parNs, busyNs, mostNs int64 // passes: the parallel phase (to the first serial turn), components' running time, the largest one's
	lastPassNs            int64                    // the latest pass's wall time (its tick's, for the slow list)
	slow                  []map[string]interface{} // ticks over simSlowTickUs since the last line
	marks                 [5]int64                 // this tick's CharList.action phases (simProfilePhase, QPC us)
	betweenDone           int64                    // this tick's ovBetweenTicks end (QPC us)
	lastPass              map[string]interface{}   // the latest pass's components, largest roots, serial turns
	serialSeen            uint64                   // serial turns counted before that pass
	lastPhases            [3]int64                 // the latest pass's parallel phase, components' running time, largest's (ns)
	lastTail              [3]int64                 // the latest pass's final flush, new helpers' first states (ns), how many
	speakerWaitUs         int64                    // the Ikemen side's waits for the speaker lock since the last line
}

const simSlowTickUs = 25000

// the speaker lock: the Ikemen side's waits for it (us, count); host audio callbacks mixed / given silence for it
var simSpeakerWaitUs, simSpeakerLocks, simSpeakerMixes, simSpeakerBusyMixes int64

// simSpeakerSite: (dev) one WithSpeakerLock by its caller - count, wait and hold (us); any thread, so under a mutex
var simSpeakerSitesMu sync.Mutex
var simSpeakerSites = map[string]*[3]int64{}

func simSpeakerSite(waitUs, holdUs int64) {
	site := "?"
	if pc, _, _, ok := runtime.Caller(2); ok {
		if f := runtime.FuncForPC(pc); f != nil {
			site = f.Name()
		}
	}
	simSpeakerSitesMu.Lock()
	r := simSpeakerSites[site]
	if r == nil {
		r = &[3]int64{}
		simSpeakerSites[site] = r
	}
	r[0]++
	r[1] += waitUs
	r[2] += holdUs
	simSpeakerSitesMu.Unlock()
}

// simGCCycles: Go GC cycles so far (runtime/metrics: cheap enough to read every tick)
var simGCSample = []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}
var simGCLast uint64

func simGCCycles() uint64 {
	metrics.Read(simGCSample)
	if simGCSample[0].Value.Kind() == metrics.KindUint64 {
		return simGCSample[0].Value.Uint64()
	}
	return 0
}

// tail: (engine thread) one pass's final effect flush and its new helpers' first states (after the join, serial)
func (s *simStatsRun) tail(flush, newHelpers time.Duration, n int) {
	s.flushNs += int64(flush)
	s.newNs += int64(newHelpers)
	s.newN += n
	s.lastTail = [3]int64{int64(flush), int64(newHelpers), int64(n)}
}

// phases: (engine thread) one pass's parallel phase, its components' summed running time and the largest one's
func (s *simStatsRun) phases(par time.Duration, busy, most int64) {
	s.parNs += int64(par)
	s.busyNs += busy
	s.mostNs += most
	s.lastPhases = [3]int64{int64(par), busy, most}
}

// the join's own work on the host thread (ovCompleteStep), summed atomically
var simStatsJoinNs, simStatsJoinN int64

// tickDone: (Ikemen thread) one tick from its wake to its done, its snapshot publication inside it (us)
func (s *simStatsRun) tickDone(us, publishUs int64) {
	s.tickUs += us
	s.publishUs += publishUs
	s.tickN++
	gc := simGCCycles()
	wait := atomic.SwapInt64(&simSpeakerWaitUs, 0)
	s.speakerWaitUs += wait
	if us > simSlowTickUs && len(s.slow) < 24 {
		end := ovSimulationStarted + us - publishUs
		ms := func(a, b int64) float64 {
			if a <= 0 || b <= 0 || b < a {
				return -1
			}
			return float64(b-a) / 1000
		}
		s.slow = append(s.slow, map[string]interface{}{"tick": ov.tick, "ms": float64(us) / 1000,
			"pass_ms": float64(s.lastPassNs) / 1e6, "publish_ms": float64(publishUs) / 1000, "gc": gc != simGCLast,
			"between": ms(ovSimulationStarted, s.betweenDone), "pre_action": ms(s.betweenDone, s.marks[0]),
			"commands": ms(s.marks[0], s.marks[1]), "prepare": ms(s.marks[1], s.marks[2]), "states": ms(s.marks[2], s.marks[3]),
			"finish": ms(s.marks[3], s.marks[4]), "post_action": ms(s.marks[4], end), "pass": s.lastPass,
			"par_ms": float64(s.lastPhases[0]) / 1e6, "busy_ms": float64(s.lastPhases[1]) / 1e6, "most_ms": float64(s.lastPhases[2]) / 1e6,
			"speaker_wait_ms": float64(wait) / 1000, "flush_ms": float64(s.lastTail[0]) / 1e6, "new_helpers_ms": float64(s.lastTail[1]) / 1e6, "new_helpers": s.lastTail[2]})
	}
	simGCLast = gc
}

var simStats = func() *simStatsRun {
	p := os.Getenv("IKEMEN_SIM_STATS")
	if p == "" {
		return nil
	}
	f, err := os.Create(p)
	if err != nil {
		return nil
	}
	return &simStatsRun{f: f, barriers: map[string]uint64{}, controllers: map[string]uint64{}}
}()

// pass: one state pass done - its graph, its component count, its char count and its wall time (engine thread)
func (s *simStatsRun) pass(g *simGraph, groups, chars int, d time.Duration) {
	s.n++
	ns := d.Nanoseconds()
	s.passNs += ns
	s.passMaxNs = Max(s.passMaxNs, ns)
	s.lastPassNs = ns
	var serial uint64
	for _, v := range s.barriers {
		serial += v
	}
	s.lastPass = map[string]interface{}{"groups": groups, "largest": g.maxRoots, "chars": chars, "serial": serial - s.serialSeen}
	s.serialSeen = serial
	s.groups += groups
	s.chars += chars
	s.maxRoots += g.maxRoots
	if g.maxRoots > s.maxRootsTop { // (the largest component at its top in this line: what joined it)
		big, best := -1, 0
		for i, r := range g.roots {
			if r > best {
				big, best = i, r
			}
		}
		s.topWhy = map[string]int{}
		for _, e := range g.edges {
			if g.find(e.A) == big {
				s.topWhy[e.Why]++
			}
		}
		s.topChars = g.chars[Max(big, 0)]
	}
	s.maxRootsTop = Max(s.maxRootsTop, g.maxRoots)
	roots := 0
	for _, r := range g.roots {
		roots += r
	}
	s.roots += roots
	if s.n < simStatsEvery {
		return
	}
	// the largest component of this pass: its roots, the dead among them, its chars, and the edges that joined it
	big, best := -1, 0
	for i, r := range g.roots {
		if r > best {
			big, best = i, r
		}
	}
	why, dead, deadAll := map[string]int{}, 0, 0
	for pn := 0; pn < MaxSimul*2; pn++ {
		if c := ovLive(pn); c != nil && !c.alive() {
			deadAll++
			if g.find(pn) == big {
				dead++
			}
		}
	}
	if big >= 0 {
		for _, e := range g.edges {
			if g.find(e.A) == big {
				why[e.Why]++
			}
		}
	}
	// what each edge kind is worth: the largest component of a graph built without it
	without := map[string]int{}
	for _, k := range []string{"custom state", "parent", "last attacker", "p2 backup", "animation owner",
		"targeted by", "bind", "target", "child", "focus", "projectile owner", "explod owner", "explod bind",
		"last attacker (dead)", "focus (dead)", "dead links", "last attacker*", "focus*"} {
		simGraphSkip = k
		without[k] = simBuildGraph(false).maxRoots
	}
	simGraphSkip = ""
	n := float64(s.n)
	per := func(m map[string]uint64) map[string]float64 {
		out := make(map[string]float64, len(m))
		for k, v := range m {
			out[k] = float64(int(float64(v)/n*100)) / 100
		}
		return out
	}
	line := map[string]interface{}{
		"tick": ov.tick, "passes": s.n,
		"pass_ms": float64(s.passNs) / n / 1e6, "pass_max_ms": float64(s.passMaxNs) / 1e6,
		"groups": float64(s.groups) / n, "chars": float64(s.chars) / n, "roots": float64(s.roots) / n,
		"dead_roots": deadAll, "explods": simStatsExplods(), "largest_roots": float64(s.maxRoots) / n, "largest_roots_top": s.maxRootsTop,
		"largest_now": map[string]interface{}{"roots": best, "dead": dead, "chars": g.chars[Max(big, 0)], "joined_by": why},
		"tick_ms": float64(s.tickUs) / float64(Max(s.tickN, 1)) / 1000, "publish_ms": float64(s.publishUs) / float64(Max(s.tickN, 1)) / 1000,
		"join_work_ms": float64(atomic.SwapInt64(&simStatsJoinNs, 0)) / float64(Max(atomic.SwapInt64(&simStatsJoinN, 0), 1)) / 1e6,
		"commands": map[string]uint64{"ticks": simCommands.ticks - s.cmd[0], "parallel": simCommands.parallelTicks - s.cmd[1],
			"inline": simCommands.inlineTicks - s.cmd[2], "fallback": simCommands.fallbackTicks - s.cmd[3]},
		"flush_kinds": func() map[string][2]float64 {
			out := map[string][2]float64{}
			for k := range s.flushKind {
				name := "other"
				if k > 0 {
					name = simLocalNames[k-1]
				}
				out[name] = [2]float64{float64(s.flushKind[k]) / n / 1e6, float64(s.flushKindN[k]) / n}
			}
			return out
		}(),
		"flush_ms": float64(s.flushNs) / n / 1e6, "newh_ms": float64(s.newNs) / n / 1e6, "newh": float64(s.newN) / n,
		"par_ms": float64(s.parNs) / n / 1e6, "busy_ms": float64(s.busyNs) / n / 1e6, "most_ms": float64(s.mostNs) / n / 1e6,
		"focus_reads": func() map[string]float64 {
			simFocusMu.Lock()
			defer simFocusMu.Unlock()
			out := per(simFocusReads)
			simFocusReads = map[string]uint64{}
			if simFocusLog {
				out["alive (all)"] = float64(atomic.SwapInt64(&simFocusAlive, 0)) / n
				out["edges: focus (dead), body running"] = float64(atomic.SwapInt64(&simDeadFocusRunning, 0)) / n
			}
			return out
		}(),
		"replay_hint": []int64{atomic.LoadInt64(&ovReplayHintChecks), atomic.LoadInt64(&ovReplayHintMisses)},
		"replay_par":  []int64{atomic.LoadInt64(&ovReplayParChecks), atomic.LoadInt64(&ovReplayParMisses)},
		"largest_top": map[string]interface{}{"roots": s.maxRootsTop, "chars": s.topChars, "joined_by": s.topWhy},
		"speaker": map[string]interface{}{"wait_ms_per_tick": float64(s.speakerWaitUs) / float64(Max(s.tickN, 1)) / 1000,
			"locks": atomic.SwapInt64(&simSpeakerLocks, 0), "mixes": atomic.SwapInt64(&simSpeakerMixes, 0),
			"silent_mixes": atomic.SwapInt64(&simSpeakerBusyMixes, 0)},
		"speaker_sites": func() map[string][3]int64 {
			simSpeakerSitesMu.Lock()
			defer simSpeakerSitesMu.Unlock()
			out := make(map[string][3]int64, len(simSpeakerSites))
			for k, v := range simSpeakerSites {
				out[k] = *v
			}
			simSpeakerSites = map[string]*[3]int64{}
			return out
		}(),
		"slow": s.slow, "largest_without": without, "serial_per_pass": per(s.barriers), "controllers_per_pass": per(s.controllers),
	}
	if b, err := json.Marshal(line); err == nil {
		s.f.Write(append(b, '\n'))
	}
	*s = simStatsRun{f: s.f, barriers: map[string]uint64{}, controllers: map[string]uint64{},
		cmd: [4]uint64{simCommands.ticks, simCommands.parallelTicks, simCommands.inlineTicks, simCommands.fallbackTicks}}
}

// simStatsExplods: live explods now, and how many of them hold a frame for good (a settled picture), by player list
func simStatsExplods() map[string]int {
	n, held := 0, 0
	for pn := range sys.explods {
		for _, e := range sys.explods[pn] {
			if e == nil || e.id == IErr {
				continue
			}
			n++
			if a := e.anim; a != nil && (len(a.frames) <= 1 || a.curFrame().Time == -1) {
				held++
			}
		}
	}
	return map[string]int{"live": n, "held_frame": held}
}

