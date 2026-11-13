package main

// Optional diagnostics on the real DLL path. No behaviour or pacing changes;
// IKEMEN_SIM_PROFILE names a job file in the caller's output directory.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

type simProfileRun struct {
	job struct {
		Out    string `json:"out"`
		Roots  int    `json:"roots"`
		Warmup int    `json:"warmup"`
		Ticks  int    `json:"ticks"`
	}
	n                          int
	active                     bool
	last                       time.Time
	file                       *os.File
	phases                     [4]int64
	groups, largestRoots       int
	edges                      []simGraphEdge
	stateCalls, stateExclusive uint64
	local                      map[string]uint64
}

var simProfiler *simProfileRun

func init() {
	path := os.Getenv("IKEMEN_SIM_PROFILE")
	if path == "" {
		return
	}
	p := &simProfileRun{}
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if err = json.Unmarshal(b, &p.job); err != nil {
		panic(err)
	}
	if p.job.Out == "" || p.job.Roots < 2 || p.job.Warmup < 0 || p.job.Ticks < 1 {
		panic("invalid simulation profile job")
	}
	simProfiler = p
}

func simProfilePhase(phase int) {
	if simStats != nil && phase >= 0 && phase < len(simStats.marks) {
		simStats.marks[phase] = ovTimingUs() // (IKEMEN_SIM_STATS: the slow ticks' phases)
	}
	if ovTickLog != nil && phase >= 0 && phase < 5 {
		ovTickMarks[phase] = ovTimingUs() // (IKEMEN_TICK_LOG: host_ov_ticklog.go)
	}
	p := simProfiler
	if p == nil {
		return
	}
	if phase == 0 {
		p.active = false
		if p.n < 0 {
			return
		}
		n := 0
		for i := range ov.slots {
			// Only ready belongs to this thread. Copying the entire slot also
			// reads loadErr/loaded while an asset worker publishes them.
			if ov.slots[i].ready {
				n++
			}
		}
		if n != p.job.Roots {
			return
		}
		if p.n == p.job.Warmup {
			p.stateCalls, p.stateExclusive = simStates.calls, simStates.exclusiveCalls
			p.local = simLocalCounts()
			simStates.maxGroups, simStates.maxRoots = 0, 0
			simStates.controllerCalls = make(map[string]uint64)
			simStates.barrierCalls = make(map[string]uint64)
			g := simInteractionGraph()
			p.groups, p.largestRoots = g.groups, g.maxRoots
			p.edges = g.edges
			if err := os.MkdirAll(p.job.Out, 0755); err != nil {
				panic(err)
			}
			var err error
			p.file, err = os.Create(filepath.Join(p.job.Out, "cpu.pprof"))
			if err != nil {
				panic(err)
			}
			if err = pprof.StartCPUProfile(p.file); err != nil {
				panic(err)
			}
		}
		p.active = true
		p.last = time.Now()
		return
	}
	if !p.active {
		return
	}
	now := time.Now()
	if p.n >= p.job.Warmup {
		p.phases[phase-1] += now.Sub(p.last).Nanoseconds()
	}
	p.last = now
	if phase != 4 {
		return
	}
	p.active = false
	p.n++
	if p.n != p.job.Warmup+p.job.Ticks {
		return
	}
	pprof.StopCPUProfile()
	p.file.Close()
	labels := []string{"commands", "prepare", "states_physics", "finish"}
	result := map[string]interface{}{"roots": p.job.Roots, "ticks": p.job.Ticks,
		"sim_workers": simWorkers, "interaction_groups": p.groups, "largest_roots": p.largestRoots,
		"interaction_edges": p.edges,
		"chars":             len(sys.charList.runOrder), "command_parallel_ticks": simCommands.parallelTicks,
		"command_fallback_ticks": simCommands.fallbackTicks, "command_max_tasks": simCommands.maxTasks,
		"command_inline_ticks": simCommands.inlineTicks, "command_max_inline_slots": simCommands.maxInlineSlots}
	result["cns_false_blocks"] = simCNSFalseBlocks
	result["state_workers"] = simStateWorkers
	result["state_calls"] = simStates.calls - p.stateCalls
	result["state_exclusive_calls"] = simStates.exclusiveCalls - p.stateExclusive
	result["state_max_groups"] = simStates.maxGroups
	result["state_max_roots"] = simStates.maxRoots
	result["state_controller_calls"] = simStates.controllerCalls
	result["state_barrier_calls"] = simStates.barrierCalls
	result["sim_state_defer"] = simStateDefer
	result["sim_state_defer_helper"] = simDeferHelper
	local := simLocalCounts()
	for k := range local {
		local[k] -= p.local[k]
	}
	result["state_local_ops"] = local
	for i, label := range labels {
		result[label+"_mean_ms"] = float64(p.phases[i]) / float64(p.job.Ticks) / 1e6
	}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(filepath.Join(p.job.Out, "phases.json"), b, 0644); err != nil {
		panic(err)
	}
	p.n = -1
}
