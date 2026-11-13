package main

// FFA command evaluation uses the existing CommandList.Step implementation.
// Inputs join before state execution. The optional FFA state pool follows in
// host_sim_state.go; collision and draw passes stay sequential after its join.
import (
	"fmt"
	"os"
	"sync"
)

const simMaxWorkers = 6

var simWorkers = 4

type simCommandJob struct {
	list                        *CommandList
	ai, helper, hpbuf, pausebuf bool
	extra                       int32
}

type simCommandGroup struct{ jobs []simCommandJob }

type simCommandPool struct {
	jobs                                chan *simCommandGroup
	done                                sync.WaitGroup
	mu                                  sync.Mutex
	failure                             interface{}
	workers                             int
	groups                              [MaxPlayerNo]simCommandGroup
	batches                             [simMaxWorkers + 1]simCommandGroup // (batch 0: the engine thread's own)
	inline                              [MaxPlayerNo]bool
	active                              bool
	ticks, parallelTicks, fallbackTicks uint64
	stepped, maxGroups                  int
	maxTasks                            int
	inlineTicks                         uint64
	maxInlineSlots                      int
}

var simCommands simCommandPool

func simConfigure(cfg map[string]interface{}) {
	simWorkers = 4
	simStateWorkers = 4
	if n, ok := cfg["sim_state_workers"].(float64); ok {
		simStateWorkers = Clamp(int(n), 0, simMaxWorkers)
	}
	// Sound controllers and the cornerpush scan stay in the component
	simStateDefer, simDeferSound, simDeferCorner = true, true, true
	if v, ok := cfg["sim_state_defer"].(bool); ok {
		simStateDefer = v
	}
	simPruneEdges = true // (host_sim_graph.go)
	if v, ok := cfg["sim_prune_edges"].(bool); ok {
		simPruneEdges = v
	}
	switch os.Getenv("IKEMEN_SIM_PRUNE") {
	case "0":
		simPruneEdges = false
	case "1":
		simPruneEdges = true
	}
	simGraphPrimedFocus = true // (host_sim_graph.go)
	if v, ok := cfg["sim_graph_primed_focus"].(bool); ok {
		simGraphPrimedFocus = v
	}
	switch os.Getenv("IKEMEN_SIM_FOCUS") {
	case "0":
		simGraphPrimedFocus = false
	case "1":
		simGraphPrimedFocus = true
	}
	// Exact (host_sim_helper.go). It was "not faster" while one component held most of the fight; with
	// simPruneEdges a 101-fighter fight's serial calls drop 7.8 -> 4.9 a pass and its tick 28.7 -> 24.7 ms
	simDeferHelper = true
	if v, ok := cfg["sim_state_defer_helper"].(bool); ok {
		simDeferHelper = v
	}
	switch os.Getenv("IKEMEN_SIM_STATE_DEFER_HELPER") {
	case "0":
		simDeferHelper = false
	case "1":
		simDeferHelper = true
	}
	switch os.Getenv("IKEMEN_SIM_STATE_DEFER") {
	case "0":
		simStateDefer = false
	case "sound": // (diagnostics: one half at a time)
		simDeferCorner = false
	case "corner":
		simDeferSound = false
	}
	simTriggerGroups = true
	if v, ok := cfg["sim_trigger_groups"].(bool); ok {
		simTriggerGroups = v
	}
	if n, ok := cfg["sim_workers"].(float64); ok {
		simWorkers = Clamp(int(n), 0, simMaxWorkers)
	}
}

func (p *simCommandPool) start() {
	if p.workers == simWorkers {
		return
	}
	if p.jobs != nil {
		close(p.jobs)
	}
	p.workers = simWorkers
	p.jobs = make(chan *simCommandGroup, MaxPlayerNo)
	for i := 0; i < p.workers; i++ {
		jobs := p.jobs
		go func() {
			for group := range jobs {
				func() {
					defer p.done.Done()
					defer func() {
						if err := recover(); err != nil {
							p.mu.Lock()
							p.failure = err
							p.mu.Unlock()
						}
					}()
					for _, job := range group.jobs {
						job.list.Step(job.ai, job.helper, job.hpbuf, job.pausebuf, job.extra)
					}
				}()
			}
		}()
	}
}

type simCommandOwner struct {
	context *CommandList
	pn      int
}

// (simBeginCommands' alias maps, cleared and reused every tick)
var simCommandOwners = map[*InputBuffer]simCommandOwner{}
var simCommandContexts = map[*CommandList]int{}

// Two different input contexts sharing a mutable buffer cannot be separated:
// a later InputUpdate/BufReset could change the inputs seen by an earlier Step.
// Aliases within a player keep that player's original immediate order. Aliases
// across players fall back for the whole tick before any command is stepped.
func simBeginCommands() {
	p := &simCommands
	p.active = false
	if !ovFFA || simWorkers < 2 || sys.netplay() {
		return
	}
	p.ticks++
	owners, contexts := simCommandOwners, simCommandContexts
	for k := range owners { // (go 1.20: the compiler turns these loops into map clears)
		delete(owners, k)
	}
	for k := range contexts {
		delete(contexts, k)
	}
	for i := range p.inline {
		p.inline[i] = false
	}
	for pn, chars := range sys.chars {
		if len(chars) == 0 || chars[0].scf(SCF_disabled) {
			continue
		}
		for _, c := range chars {
			if len(c.cmd) == 0 {
				continue
			}
			// A destroyed helper (helperIndex -1) waits in its slot for reuse with the command lists it had - the root's,
			// for most helpers - and commandUpdate never updates, resets or steps them (it needs helperIndex 0, or > 0
			// with lists of its own). It touches no buffer in this pass, so it aliases nothing; counted, it put every
			// player with a dead helper inline on the engine thread.
			if c.helperIndex < 0 {
				continue
			}
			context := &c.cmd[0]
			if c.helperIndex > 0 && len(chars[0].cmd) > 0 && context == &chars[0].cmd[0] {
				continue
			}
			if previous, found := contexts[context]; found {
				if previous != pn {
					p.fallbackTicks++
					return
				}
				p.inline[pn] = true
				continue
			}
			contexts[context] = pn
			// Every list of a char's cmd shares cmd[0]'s buffer - each place that makes a cmd slice (the CMD compiler,
			// a keyctrl helper's own lists, state_clone) gives all of its lists one buffer, nothing assigns a list's
			// Buffer afterwards, and whole lists are only ever copied by CopyList (which leaves Buffer). The walk over
			// all MaxPlayerNo lists that skipped every list after the first as "the common buffer" read 128 strided
			// lists per root for nothing; cmd[0]'s buffer is the one this char brings.
			if buffer := c.cmd[0].Buffer; buffer != nil {
				if previous, ok := owners[buffer]; ok && previous.context != context {
					if previous.pn != pn {
						p.fallbackTicks++
						return
					}
					p.inline[pn] = true
				}
				owners[buffer] = simCommandOwner{context, pn}
			}
		}
	}
	inline := 0
	for _, value := range p.inline {
		if value {
			inline++
		}
	}
	if inline > 0 {
		p.inlineTicks++
	}
	if inline > p.maxInlineSlots {
		p.maxInlineSlots = inline
	}
	p.start()
	for i := range p.groups {
		p.groups[i].jobs = p.groups[i].jobs[:0]
	}
	p.stepped = 0
	p.active = true
}

func simStepCommand(pn int, cl *CommandList, ai, helper, hpbuf, pausebuf bool, extra int32) {
	if !simCommands.active || simCommands.inline[pn] {
		cl.Step(ai, helper, hpbuf, pausebuf, extra)
		return
	}
	group := &simCommands.groups[pn]
	group.jobs = append(group.jobs, simCommandJob{cl, ai, helper, hpbuf, pausebuf, extra})
	simCommands.stepped++
}

func simFinishCommands() {
	p := &simCommands
	if !p.active {
		return
	}
	p.active = false
	n := 0
	for i := range p.groups {
		if len(p.groups[i].jobs) > 0 {
			n++
		}
	}
	if n > p.maxGroups {
		p.maxGroups = n
	}
	// Very small workloads cost more to dispatch than to evaluate inline.
	if n < 4 {
		for i := range p.groups {
			for _, job := range p.groups[i].jobs {
				job.list.Step(job.ai, job.helper, job.hpbuf, job.pausebuf, job.extra)
			}
		}
		return
	}
	// Coalesce independent slot groups into at most one task per worker. A
	// helper/foreign-context list remains ordered with its slot. This avoids
	// a channel send/WaitGroup update for each of the fight's 101 roots.
	// (the engine thread takes a share itself - batch 0 - instead of waiting idle for the workers; which thread
	// steps an independent group changes nothing)
	parts := p.workers + 1
	var weights [simMaxWorkers + 1]int
	for i := range p.batches {
		p.batches[i].jobs = p.batches[i].jobs[:0]
	}
	for i := range p.groups {
		group := &p.groups[i]
		if len(group.jobs) == 0 {
			continue
		}
		worker := 0
		for w := 1; w < parts; w++ {
			if weights[w] < weights[worker] {
				worker = w
			}
		}
		p.batches[worker].jobs = append(p.batches[worker].jobs, group.jobs...)
		for _, job := range group.jobs {
			weights[worker] += len(job.list.Commands)
		}
	}
	tasks := 0 // (tasks handed to workers: batch 0 stays here)
	for i := 1; i < parts; i++ {
		if len(p.batches[i].jobs) > 0 {
			tasks++
		}
	}
	if tasks > p.maxTasks {
		p.maxTasks = tasks
	}
	p.parallelTicks++
	for i := 1; i < parts; i++ {
		if len(p.batches[i].jobs) > 0 {
			p.done.Add(1)
			p.jobs <- &p.batches[i]
		}
	}
	func() { // (batch 0 here; a panic is recorded like a worker's, after every worker has finished)
		defer func() {
			if err := recover(); err != nil {
				p.mu.Lock()
				p.failure = err
				p.mu.Unlock()
			}
		}()
		for _, job := range p.batches[0].jobs {
			job.list.Step(job.ai, job.helper, job.hpbuf, job.pausebuf, job.extra)
		}
	}()
	p.done.Wait()
	if p.failure != nil {
		panic(fmt.Sprintf("FFA command worker: %v", p.failure))
	}
}
