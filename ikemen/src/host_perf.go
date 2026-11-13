package main

// Opt-in, in-engine simulation benchmark. No converted assets or JSON
// frame transport. The oracle is deliberately outside the measured tick.
import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"sort"
	"time"
)

var hostPerfOn = os.Getenv("IKEMEN_PERF") != ""

type perfJob struct {
	Out          string `json:"out"`
	Seed         int32  `json:"seed"`
	Warmup       int    `json:"warmup"`
	Ticks        int    `json:"ticks"`
	Oracle       bool   `json:"oracle"`
	Profile      bool   `json:"profile"`
	StartState   int32  `json:"start_state"`
	FFA          bool   `json:"ffa"`
	Workers      int    `json:"sim_workers"`
	StateWorkers int    `json:"sim_state_workers"`
	Layout       string `json:"layout"`
	Graph        bool   `json:"graph"`
}

// Enable FFA before the normal loader compiles the command contexts. The
// benchmark still uses Ikemen's own loader, original state code and tick loop.
func init() {
	if !hostPerfOn {
		return
	}
	b, err := os.ReadFile(os.Getenv("IKEMEN_PERF"))
	if err != nil {
		return
	} // unit-test executables use the sentinel "unit-tests"
	var job perfJob
	if json.Unmarshal(b, &job) == nil && job.FFA {
		ovConfigure(map[string]interface{}{"ffa": true, "sim_workers": float64(job.Workers), "sim_state_workers": float64(job.StateWorkers)})
	}
	// Diagnostic for bounded hidden runs: one goroutine dump after N seconds
	// (written to stderr, i.e. the run's engine.log). Never set by default.
	if d, err := time.ParseDuration(os.Getenv("IKEMEN_PERF_STACKS")); err == nil && d > 0 {
		time.AfterFunc(d, func() {
			buf := make([]byte, 32<<20)
			n := runtime.Stack(buf, true)
			fmt.Fprintf(os.Stderr, "IKEMEN_PERF goroutine dump after %v\n%s\n", d, buf[:n])
		})
	}
}

type perfRun struct {
	job                               perfJob
	n                                 int
	start, boot                       time.Time
	ns                                []int64
	profile                           *os.File
	digest                            *os.File
	writer                            *bufio.Writer
	buf                               []byte
	fields                            map[reflect.Type][]int
	seen                              map[perfRef]uint64
	stateNos                          [MaxPlayerNo][]int
	peakChars, peakExplods, peakProjs int
	roots, stateChanges               int
	lastStates                        map[int32]int32
	initialLife, finalLife            int64
	mem                               runtime.MemStats
	cpu0, cpu1                        float64 // process CPU seconds at the end of warmup / of the measured ticks
	commandStart                      time.Time
	commandTick                       int64
	commandNS                         []int64
	logicFrame                        bool
	worldTickBefore                   int
	logicTicks, nonUnitTickAdvances   int
	groupCounts, largestGroups        []int
}

var hostPerf *perfRun

type perfRef struct {
	typeOf reflect.Type
	ptr    uintptr
	length int
}

func hostPerfBegin() {
	r := &perfRun{boot: time.Now(), lastStates: make(map[int32]int32), fields: make(map[reflect.Type][]int), seen: make(map[perfRef]uint64)}
	b, err := os.ReadFile(os.Getenv("IKEMEN_PERF"))
	if err != nil {
		panic(err)
	}
	if err = json.Unmarshal(b, &r.job); err != nil {
		panic(err)
	}
	if r.job.Ticks < 1 || r.job.Warmup < 0 || r.job.Out == "" {
		panic("invalid IKEMEN_PERF job")
	}
	if err = os.MkdirAll(r.job.Out, 0755); err != nil {
		panic(err)
	}
	if r.job.Oracle {
		r.digest, err = os.Create(filepath.Join(r.job.Out, "digest.tsv"))
		if err != nil {
			panic(err)
		}
		r.writer = bufio.NewWriterSize(r.digest, 64*1024)
	}
	r.ns = make([]int64, 0, r.job.Ticks)
	hostPerf = r
	// One deterministic initial scenario, through the normal loaded match.
	Srand(r.job.Seed)
	sys.roundNo, sys.intro = 2, 0
	sys.maxRoundTime, sys.curRoundTime = -1, -1
	sys.matchWins = [2]int32{99, 99}
	// Stock HUD layouts have only eight slots. Keep their first four per side
	// in this simulation-only benchmark; all roots still simulate normally.
	for side := range sys.fightScreen.teamOrder {
		if len(sys.fightScreen.teamOrder[side]) > 4 {
			sys.fightScreen.teamOrder[side] = sys.fightScreen.teamOrder[side][:4]
		}
	}
	for pn := range sys.cgi {
		for sn := range sys.cgi[pn].states {
			r.stateNos[pn] = append(r.stateNos[pn], int(sn))
		}
		sort.Ints(r.stateNos[pn])
	}
	for pn, slot := range sys.chars {
		if len(slot) == 0 {
			continue
		}
		c := slot[0]
		r.roots++
		r.initialLife += int64(c.life)
		c.varSet(48, 0)
		ctrl := int32(1)
		if r.job.StartState != 0 {
			ctrl = -1 // Let the actual StateDef determine control in targeted cases.
		}
		c.selfState(r.job.StartState, -1, -1, ctrl, "")
		if r.job.FFA {
			pair := pn / 2
			x, z := float32(pair%8)*470-1700, float32(pair/8)*160-550
			if pn%2 != 0 {
				x += 110
			}
			if r.job.Layout == "idle" {
				x = float32(pn%13)*310 - 1900
				z = float32(pn/13)*150 - 550
			}
			if r.job.Layout == "melee" {
				x = float32(pn%13)*90 - 550
				z = float32(pn/13)*20 - 70
			}
			oldX, oldZ := c.pos[0], c.pos[2]
			if r.job.Layout == "isolated" {
				x, z = -60+float32(pn%2)*110, -450+float32(pair)*300
			}
			c.setPosX(x*ovWPP()/c.localscl, true)
			c.setPosZ(z*ovWPP()/c.localscl, true)
			if r.job.Layout == "isolated" {
				// Keep already-created intro helpers with their own root when
				// constructing the four separated depth-lane test pairs.
				for _, h := range slot[1:] {
					h.setPosX(h.pos[0]+(c.pos[0]-oldX)*c.localscl/h.localscl, true)
					h.setPosZ(h.pos[2]+(c.pos[2]-oldZ)*c.localscl/h.localscl, true)
				}
			}
			c.setFacing(1 - float32(pn%2)*2)
			if pn^1 < len(sys.chars) && len(sys.chars[pn^1]) > 0 {
				ov.slots[pn].focus = pn ^ 1
			}
		}
	}
	fmt.Fprintf(os.Stderr, "IKEMEN_PERF begin seed=%d warmup=%d ticks=%d oracle=%v profile=%v\n", r.job.Seed, r.job.Warmup, r.job.Ticks, r.job.Oracle, r.job.Profile)
}

func hostPerfBefore() {
	r := hostPerf
	if r.job.Graph && ovFFA {
		g := simInteractionGraph()
		if r.n >= r.job.Warmup {
			r.groupCounts = append(r.groupCounts, g.groups)
			r.largestGroups = append(r.largestGroups, g.maxRoots)
		}
	}
	if r.n == r.job.Warmup {
		runtime.ReadMemStats(&r.mem)
		r.cpu0 = hostProcessCPU()
		if r.job.Profile {
			var err error
			r.profile, err = os.Create(filepath.Join(r.job.Out, "cpu.pprof"))
			if err != nil {
				panic(err)
			}
			if err = pprof.StartCPUProfile(r.profile); err != nil {
				panic(err)
			}
		}
	}
	// Keep draw-cue/afterimage sampling enabled, even though no GPU draw follows.
	sys.frameSkip = false
	r.logicFrame, r.worldTickBefore = sys.tickFrame(), sys.tickCount
	r.commandTick = 0
	r.start = time.Now()
}

func hostPerfCommandBefore() {
	if hostPerf != nil {
		hostPerf.commandStart = time.Now()
	}
}

func hostPerfCommandAfter() {
	if hostPerf != nil {
		hostPerf.commandTick += time.Since(hostPerf.commandStart).Nanoseconds()
	}
}

func hostPerfAfter() bool {
	r := hostPerf
	d := time.Since(r.start).Nanoseconds()
	if r.n >= r.job.Warmup {
		r.ns = append(r.ns, d)
		r.commandNS = append(r.commandNS, r.commandTick)
		if r.logicFrame {
			r.logicTicks++
		}
		if sys.tickCount-r.worldTickBefore != 1 {
			r.nonUnitTickAdvances++
		}
	}
	chars, explods, projs := len(sys.charList.runOrder), 0, 0
	for pn := range sys.chars {
		for _, e := range sys.explods[pn] {
			if e != nil && e.id != IErr {
				explods++
			}
		}
		for _, p := range sys.projs[pn] {
			if p != nil && p.isActive() {
				projs++
			}
		}
	}
	if chars > r.peakChars {
		r.peakChars = chars
	}
	if explods > r.peakExplods {
		r.peakExplods = explods
	}
	if projs > r.peakProjs {
		r.peakProjs = projs
	}
	for _, c := range sys.charList.runOrder {
		if previous, ok := r.lastStates[c.id]; ok && previous != c.ss.no {
			r.stateChanges++
		}
		r.lastStates[c.id] = c.ss.no
	}
	if r.job.Oracle && r.n >= r.job.Warmup {
		r.buf = r.buf[:0]
		for key := range r.seen {
			delete(r.seen, key)
		}
		r.value(reflect.ValueOf(&sys.SystemStateVars).Elem())
		for pn := range sys.chars {
			r.value(reflect.ValueOf(sys.chars[pn]))
			for _, c := range sys.chars[pn] {
				r.commands(c.cmd)
			}
			r.value(reflect.ValueOf(sys.explods[pn]))
			r.value(reflect.ValueOf(sys.projs[pn]))
			// Persistent controller counters can outlive the current state.
			for _, sn := range r.stateNos[pn] {
				r.word(uint64(sn))
				r.value(reflect.ValueOf(sys.cgi[pn].states[int32(sn)].ctrlsps))
			}
		}
		r.value(reflect.ValueOf(sys.allPalFX))
		r.value(reflect.ValueOf(sys.bgPalFX))
		r.value(reflect.ValueOf(sys.afterImageCount))
		// Evaluation/collision order is part of the simulation contract.
		for _, c := range sys.charList.runOrder {
			r.word(uint64(c.id))
		}
		digest := sha256.Sum256(r.buf)
		fmt.Fprintf(r.writer, "%d\t%x\t%d\t%d\t%d\t%d\n", r.n, digest, chars, explods, projs, sys.randseed)
		if s := os.Getenv("IKEMEN_PERF_FIELD_TICK"); s != "" && s == fmt.Sprint(r.n) {
			r.fieldDump()
		}
		if (r.n-r.job.Warmup)%60 == 0 {
			if err := r.writer.Flush(); err != nil {
				panic(err)
			}
		}
	}
	if r.n%120 == 0 {
		fmt.Fprintf(os.Stderr, "IKEMEN_PERF tick=%d chars=%d explods=%d\n", r.n, chars, explods)
	}
	r.n++
	if r.n < r.job.Warmup+r.job.Ticks {
		return false
	}
	r.cpu1 = hostProcessCPU()
	if r.profile != nil {
		pprof.StopCPUProfile()
		r.profile.Close()
	}
	if r.writer != nil {
		if err := r.writer.Flush(); err != nil {
			panic(err)
		}
		if err := r.digest.Close(); err != nil {
			panic(err)
		}
	}
	timings, err := os.Create(filepath.Join(r.job.Out, "timings.tsv"))
	if err != nil {
		panic(err)
	}
	w := bufio.NewWriter(timings)
	for i, ns := range r.ns {
		var commands int64
		if i < len(r.commandNS) {
			commands = r.commandNS[i]
		}
		fmt.Fprintf(w, "%d\t%.6f\t%.6f\t%.6f\n", r.job.Warmup+i, float64(ns)/1e6, float64(commands)/1e6, float64(ns-commands)/1e6)
	}
	if err := w.Flush(); err != nil {
		panic(err)
	}
	if err := timings.Close(); err != nil {
		panic(err)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if r.job.Profile {
		f, err := os.Create(filepath.Join(r.job.Out, "allocs.pprof"))
		if err != nil {
			panic(err)
		}
		if err := pprof.Lookup("allocs").WriteTo(f, 0); err != nil {
			panic(err)
		}
		if err := f.Close(); err != nil {
			panic(err)
		}
	}
	for _, slot := range sys.chars {
		if len(slot) > 0 {
			r.finalLife += int64(slot[0].life)
		}
	}
	sorted := append([]int64(nil), r.ns...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, n := range r.ns {
		sum += n
	}
	commandSorted := append([]int64(nil), r.commandNS...)
	nonCommandSorted := make([]int64, len(r.ns))
	var commandSum int64
	for i, n := range r.commandNS {
		commandSum += n
		nonCommandSorted[i] = r.ns[i] - n
	}
	sort.Slice(commandSorted, func(i, j int) bool { return commandSorted[i] < commandSorted[j] })
	sort.Slice(nonCommandSorted, func(i, j int) bool { return nonCommandSorted[i] < nonCommandSorted[j] })
	result := map[string]any{
		"schema": 4, "seed": r.job.Seed, "ticks": len(r.ns), "warmup": r.job.Warmup,
		"oracle": r.job.Oracle, "profile": r.job.Profile, "max_simul": MaxSimul,
		"median_ms":              float64(sorted[len(sorted)/2]) / 1e6,
		"p95_ms":                 float64(sorted[(len(sorted)-1)*95/100]) / 1e6,
		"mean_ms":                float64(sum) / float64(len(sorted)) / 1e6,
		"command_median_ms":      float64(commandSorted[len(commandSorted)/2]) / 1e6,
		"command_mean_ms":        float64(commandSum) / float64(len(sorted)) / 1e6,
		"non_command_median_ms":  float64(nonCommandSorted[len(nonCommandSorted)/2]) / 1e6,
		"non_command_p95_ms":     float64(nonCommandSorted[(len(nonCommandSorted)-1)*95/100]) / 1e6,
		"non_command_mean_ms":    float64(sum-commandSum) / float64(len(sorted)) / 1e6,
		"start_state":            r.job.StartState,
		"logic_ticks":            r.logicTicks,
		"non_unit_tick_advances": r.nonUnitTickAdvances,
		"peak_chars":             r.peakChars, "peak_explods": r.peakExplods, "peak_projectiles": r.peakProjs,
		"roots": r.roots, "state_changes": r.stateChanges,
		"initial_life": r.initialLife, "final_life": r.finalLife,
		"allocated_bytes": after.TotalAlloc - r.mem.TotalAlloc, "gc_cycles": after.NumGC - r.mem.NumGC,
		"match_wall_s": time.Since(r.boot).Seconds(), "go": runtime.Version(), "gomaxprocs": runtime.GOMAXPROCS(0),
		"ffa": ovFFA, "sim_workers": simWorkers, "layout": r.job.Layout,
		"sim_state_workers": simStateWorkers, "state_calls": simStates.calls, "state_exclusive_calls": simStates.exclusiveCalls,
		"state_max_groups": simStates.maxGroups, "state_max_roots": simStates.maxRoots,
		"command_parallel_ticks": simCommands.parallelTicks, "command_fallback_ticks": simCommands.fallbackTicks,
		"command_max_groups":   simCommands.maxGroups,
		"command_max_tasks":    simCommands.maxTasks,
		"command_inline_ticks": simCommands.inlineTicks, "command_max_inline_slots": simCommands.maxInlineSlots,
		"cns_false_blocks": simCNSFalseBlocks,
		"cpu_ms_per_tick":  (r.cpu1 - r.cpu0) * 1000 / float64(len(sorted)),
		"sim_state_defer":  simStateDefer, "sim_state_defer_helper": simDeferHelper, "state_helper_miss": simHelperMiss, "state_local_ops": simLocalCounts(),
	}
	if len(r.groupCounts) > 0 {
		sort.Ints(r.groupCounts)
		sort.Ints(r.largestGroups)
		result["interaction_groups_median"] = r.groupCounts[len(r.groupCounts)/2]
		result["interaction_groups_min"] = r.groupCounts[0]
		result["largest_interaction_group_median"] = r.largestGroups[len(r.largestGroups)/2]
		result["largest_interaction_group_max"] = r.largestGroups[len(r.largestGroups)-1]
	}
	b, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(filepath.Join(r.job.Out, "summary.json"), b, 0644); err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stderr, "IKEMEN_PERF", string(b))
	return true
}

// Canonical binary oracle: scalar bit patterns, slice order and sorted map keys.
// Covers character/helper fields, variables, hitdefs/gethitvars, command buffers,
// animation cursors, explods/projectiles, RNG, AI and global tick/round state.
// Excludes immutable compiled code/assets and GPU resources, not runtime state.
// Reflection is used only in verification runs, never in a performance run.
func (r *perfRun) word(v uint64) { r.buf = binary.LittleEndian.AppendUint64(r.buf, v) }

func (r *perfRun) value(v reflect.Value) {
	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			r.word(1)
		} else {
			r.word(0)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		r.word(uint64(v.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		r.word(v.Uint())
	case reflect.Float32:
		r.word(uint64(math.Float32bits(float32(v.Float()))))
	case reflect.Float64:
		r.word(math.Float64bits(v.Float()))
	case reflect.String:
		s := v.String()
		r.word(uint64(len(s)))
		r.buf = append(r.buf, s...)
	case reflect.Pointer:
		if v.IsNil() {
			r.word(0)
			return
		}
		r.word(1)
		// Asset pointers are opaque. Aliasing of mutable state is serialized
		// using encounter-order IDs, never process-dependent addresses.
		switch v.Type().Elem().Name() {
		case "Sff", "Sprite", "PaletteList":
			return
		}
		if r.reference(v, -1) {
			return
		}
		r.value(v.Elem())
	case reflect.Interface:
		if !v.IsNil() {
			r.value(v.Elem())
		}
	case reflect.Slice:
		r.word(uint64(v.Len()))
		if v.Len() > 0 && r.reference(v, v.Len()) {
			return
		}
		for i := 0; i < v.Len(); i++ {
			r.value(v.Index(i))
		}
	case reflect.Array:
		r.word(uint64(v.Len()))
		for i := 0; i < v.Len(); i++ {
			r.value(v.Index(i))
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Kind() == reflect.String {
				return keys[i].String() < keys[j].String()
			}
			return keys[i].Int() < keys[j].Int()
		})
		r.word(uint64(len(keys)))
		for _, k := range keys {
			r.value(k)
			r.value(v.MapIndex(k))
		}
	case reflect.Struct:
		t := v.Type()
		// Resource identity/content cannot vary during simulation. StateBytecode
		// is slot-owned compiled code; StateState alongside it contains live data.
		switch t.Name() {
		case "Sff", "Sprite", "PaletteList":
			return
		case "StateBytecode":
			r.value(v.FieldByName("playerNo"))
			r.value(v.FieldByName("ctrlsps"))
			return
		}
		fields, ok := r.fields[t]
		if !ok {
			for i := 0; i < v.NumField(); i++ {
				f := t.Field(i)
				if t.Name() == "Char" && f.Name == "cmd" {
					continue
				} // typed below, includes every list
				if t.Name() == "Char" && f.Name == "ovStand" {
					continue
				} // render-side cache (huge-sprite rule), not simulation state
				if t.Name() == "Explod" && f.Name == "ovWit" {
					continue
				} // a debug serial numbered as explods are made, so in parallel
				// the state pool's components take them in thread order; nothing in the simulation reads it
				if t.Name() == "Animation" && (f.Name == "frames" || f.Name == "interpolate_offset" || f.Name == "interpolate_scale" || f.Name == "interpolate_angle" || f.Name == "interpolate_blend") {
					continue
				}
				if t.Name() == "CommandList" && f.Name == "Names" {
					continue
				} // immutable command definitions
				if t.Name() == "Command" {
					switch f.Name {
					case "curtime", "curbuftime", "cursteptime", "completeframe", "completed", "stepTimers", "loopOrder":
					default:
						continue // immutable command definitions
					}
				}
				fields = append(fields, i)
			}
			r.fields[t] = fields
		}
		for _, i := range fields {
			r.value(v.Field(i))
		}
	default:
		panic("unsupported oracle field: " + v.Type().String())
	}
}

// Command lists dominate oracle volume at 128 slots. Serialize every mutable
// command field directly, including foreign-state command lists; helpers that
// share a root's list retain a canonical alias reference. No reflection per
// command and no omitted command buffers.
func (r *perfRun) commands(lists []CommandList) {
	r.word(uint64(len(lists)))
	if len(lists) == 0 || r.reference(reflect.ValueOf(lists), len(lists)) {
		return
	}
	for i := range lists {
		cl := &lists[i]
		r.value(reflect.ValueOf(cl.Buffer))
		r.word(uint64(len(cl.Commands)))
		for j := range cl.Commands {
			r.word(uint64(len(cl.Commands[j])))
			for k := range cl.Commands[j] {
				c := &cl.Commands[j][k]
				r.buf = binary.LittleEndian.AppendUint32(r.buf, uint32(c.curtime))
				r.buf = binary.LittleEndian.AppendUint32(r.buf, uint32(c.curbuftime))
				r.buf = binary.LittleEndian.AppendUint32(r.buf, uint32(c.cursteptime))
				if c.completeframe {
					r.buf = append(r.buf, 1)
				} else {
					r.buf = append(r.buf, 0)
				}
				r.word(uint64(len(c.completed)))
				for _, b := range c.completed {
					if b {
						r.buf = append(r.buf, 1)
					} else {
						r.buf = append(r.buf, 0)
					}
				}
				r.word(uint64(len(c.stepTimers)))
				for _, n := range c.stepTimers {
					r.buf = binary.LittleEndian.AppendUint32(r.buf, uint32(n))
				}
				r.word(uint64(len(c.loopOrder)))
				for _, n := range c.loopOrder {
					r.word(uint64(n))
				}
			}
		}
	}
}

func (r *perfRun) reference(v reflect.Value, length int) bool {
	key := perfRef{v.Type(), v.Pointer(), length}
	if id, ok := r.seen[key]; ok {
		r.word(2)
		r.word(id)
		return true
	}
	r.seen[key] = uint64(len(r.seen) + 1)
	r.word(1)
	return false
}

// Diagnostics for a digest mismatch (IKEMEN_PERF_FIELD_TICK=n): one hash per
// global block and per character field at tick n, each hashed on its own.
func (r *perfRun) fieldDump() {
	f, err := os.Create(filepath.Join(r.job.Out, "fields.tsv"))
	if err != nil {
		panic(err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	hash := func(v reflect.Value) [32]byte {
		r.buf = r.buf[:0]
		for key := range r.seen {
			delete(r.seen, key)
		}
		r.value(v)
		return sha256.Sum256(r.buf)
	}
	sv := reflect.ValueOf(&sys.SystemStateVars).Elem()
	for i := 0; i < sv.NumField(); i++ {
		fmt.Fprintf(w, "sys\t%s\t%x\n", sv.Type().Field(i).Name, hash(sv.Field(i)))
	}
	for pn := range sys.chars {
		for ci, c := range sys.chars[pn] {
			cv := reflect.ValueOf(c).Elem()
			for i := 0; i < cv.NumField(); i++ {
				name := cv.Type().Field(i).Name
				if name == "cmd" {
					continue
				}
				fmt.Fprintf(w, "char\t%d.%d.%d.%d\t%s\t%x\n", pn, ci, c.id, c.ss.no, name, hash(cv.Field(i)))
			}
			keys := make([]string, 0, len(c.mapArray))
			for k := range c.mapArray {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "map\t%d.%d.%d\t%s\t%v\n", pn, ci, c.id, k, c.mapArray[k])
			}
		}
		fmt.Fprintf(w, "explods\t%d\t%x\n", pn, hash(reflect.ValueOf(sys.explods[pn])))
		// (each explod's fields on their own, in list order: which field of which explod differs)
		for ei, e := range sys.explods[pn] {
			if e == nil {
				continue
			}
			ev := reflect.ValueOf(e).Elem()
			for i := 0; i < ev.NumField(); i++ {
				fmt.Fprintf(w, "explod\t%d.%d.%d\t%s\t%x\n", pn, ei, e.id, ev.Type().Field(i).Name, hash(ev.Field(i)))
			}
		}
		fmt.Fprintf(w, "projs\t%d\t%x\n", pn, hash(reflect.ValueOf(sys.projs[pn])))
	}
}
