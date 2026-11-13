package main

// The parallel explod update. With many fighters, thousands of explods can be alive at once (most of them holding a
// frame for good), and System.explodUpdate's walk over them one by one on the engine thread is nearly all first-touch
// cache misses (the explod, its animation, its frames).
// Explod.update of one explod writes only that explod and what it owns alone, so most of them are updated on a few
// goroutines at once, the rest in storage order after them:
//
//   - What one explod's update writes: its own fields and its own *Animation (every explod's is a fresh copy:
//     AnimationTable.get, Char.getAnim; nothing shares one between explods). Beyond that only (1) a synced explod
//     (syncId > 0) writes its sync char (enableSyncId, getPalfx may make the char's PalFX, its aimg settings) - those
//     run serially; (2) a custom shader's textures step their own animations (ShaderTexture.step, possibly shared) -
//     serially; (3) an own PalFX steps (PalFX.step) - that PalFX can be a char's (taken by a sync) or another explod's,
//     so its step is not taken by the worker but reported, and taken in phase 3 (below); (4) a missing sprite's one
//     warning (Animation.UpdateSprite: the
//     SFF's debugMissing map) - under ovMissingMu; (5) IKEMEN_OV_SNDLOG's log - with it on, the whole update stays
//     serial.
//   - What it reads: chars (flags, state number, acttmp, interPos, size, offset), globals (tick,
//     camera, pause timers) and the frozen id index - none written by any explod's update, except the sync writes of
//     (1), which no explod outside phase 2 reads (enableSyncId, a char's PalFX pointer, aimg).
//   - The PalFX steps (phase 3): every PalFX operation inside the explod update is PalFX.step - a function of that
//     PalFX's own fields and sys.tickFrame(), fixed for the pass (nothing after the step in Explod.update reads the
//     PalFX; no explod's update reads another's PalFX contents: a sync takes only the pointer) - so a PalFX ends as
//     step applied n times to where it began, n the explods that step it, whoever steps it and in whatever order. Phase
//     3 hands each reported PalFX's steps to one goroutine, chosen by the PalFX's address, so no two steps of one PalFX
//     ever run at once; the serial explods (phase 2) take their own steps inline, before phase 3 starts.
//   - So phase 1 (parallel), phase 2 (storage order: the serial explods' full update) and phase 3 (the reported steps)
//     leave every explod, animation and PalFX exactly as the walk in storage order did. Removal (id = IErr) is noted per
//     explod as its update ends - nothing changes an id between that and the prune - and the prune keeps the same
//     explods in the same order (System.explodPrune's stable swap, from the notes instead of reading every explod
//     again: another cache miss each).
//
// A panic in a worker is carried to the engine thread. Env IKEMEN_EXPLOD_PAR=0: the serial walk (A/B).

import (
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"
)

var ovExplodParOn = os.Getenv("IKEMEN_EXPLOD_PAR") != "0"

const ovExplodParBlock = 32 // explods a worker takes at a time

// ovExplodParMin: fewer explods - the serial walk (the explod update and cue); ovCharParMin: fewer chars - the chars'
// cue values on the spot. Dev env IKEMEN_PAR_MIN=<n> sets both (1: always parallel - for checks on small fights)
var ovExplodParMin, ovCharParMin = func() (int, int) {
	if n, err := strconv.Atoi(os.Getenv("IKEMEN_PAR_MIN")); err == nil && n >= 1 {
		return n, n
	}
	return 128, 64
}()

// ovExplodParMax: goroutines (the engine thread one of them) of the parallel explod update and cue - dev env
// IKEMEN_EXPLOD_PAR_WORKERS (1-8)
var ovExplodParMax = func() int {
	if n, err := strconv.Atoi(os.Getenv("IKEMEN_EXPLOD_PAR_WORKERS")); err == nil && n >= 1 && n <= 8 {
		return n
	}
	return 4
}()

// per explod (flat index): what phase 1 left for phase 2
const (
	ovExSerial uint8 = 1 << iota // not updated: phase 2 updates it in full
	ovExPal                      // updated, its own PalFX's step still to take
	ovExGone                     // removed (id IErr) - left out by the prune
)

var ovMissingMu sync.Mutex // (Animation.UpdateSprite's missing-sprite warning)

var ovExplodFlat []*Explod
var ovExplodNote []uint8
var ovExplodPal []*PalFX // (per flat index: the PalFX whose step it reported)

// ovExplodParSafe: e's update writes nothing but e and what e alone owns (its PalFX step reported, not taken)
func ovExplodParSafe(e *Explod) bool {
	return e.syncId <= 0 && e.customShader.name == ""
}

// explodUpdatePar: System.explodUpdate's walk and prune, in parallel; false: not taken (the serial walk runs)
func (s *System) explodUpdatePar() bool {
	if !ovExplodParOn || ovSndLog {
		return false
	}
	n := 0
	for i := range s.explods {
		n += len(s.explods[i])
	}
	if n < ovExplodParMin {
		return false
	}
	flat := ovExplodFlat[:0]
	for i := range s.explods {
		flat = append(flat, s.explods[i]...)
	}
	note := ovExplodNote
	if cap(note) < n {
		note = make([]uint8, ovParCap(n))
	}
	note = note[:n]
	pal := ovExplodPal
	if cap(pal) < n {
		pal = make([]*PalFX, ovParCap(n))
	}
	pal = pal[:n]

	// Phase 1: every explod that is safe, on up to ovExplodParMax goroutines
	var next atomic.Int64
	var failMu sync.Mutex
	var failed interface{}
	work := func() {
		defer func() {
			if r := recover(); r != nil {
				failMu.Lock()
				if failed == nil {
					failed = r
				}
				failMu.Unlock()
				next.Store(int64(n)) // (the others stop taking blocks)
			}
		}()
		for {
			lo := int(next.Add(ovExplodParBlock)) - ovExplodParBlock
			if lo >= n {
				return
			}
			hi := Min(lo+ovExplodParBlock, n)
			for k := lo; k < hi; k++ {
				e := flat[k]
				if !ovExplodParSafe(e) {
					note[k] = ovExSerial
					continue
				}
				var f uint8
				if e.updateStep(true) {
					f |= ovExPal
					pal[k] = e.palfx
				}
				if e.id == IErr {
					f |= ovExGone
				}
				note[k] = f
			}
		}
	}
	workers := Min(ovExplodParMax, runtime.GOMAXPROCS(0))
	if b := (n + ovExplodParBlock - 1) / ovExplodParBlock; b < workers {
		workers = b
	}
	ovParRun(workers, func(int) { work() })
	if failed != nil {
		panic(failed)
	}

	// Phase 2: in storage order - the serial explods' update
	steps := 0
	for k, e := range flat {
		switch f := note[k]; {
		case f&ovExSerial != 0:
			e.update()
			if e.id == IErr {
				note[k] |= ovExGone
			}
		case f&ovExPal != 0:
			steps++
		}
	}

	// Phase 3: the reported PalFX steps, each PalFX's on the one goroutine its address picks
	if steps > 0 {
		pw := workers
		if steps < 2*ovExplodParBlock {
			pw = 1
		}
		ovParRun(pw, func(w int) {
			for k, pf := range pal {
				if note[k]&ovExPal != 0 && (pw == 1 || ovPalWorker(pf, pw) == w) {
					pf.step()
				}
			}
		})
	}

	// The prune (System.explodPrune's stable swap, per player, from the notes)
	k := 0
	for i := range s.explods {
		list := s.explods[i]
		w := 0
		for j := range list {
			if note[k+j]&ovExGone == 0 {
				list[w], list[j] = list[j], list[w]
				w++
			}
		}
		k += len(list)
		s.explods[i] = list[:w]
	}

	for i := range flat {
		flat[i], pal[i] = nil, nil
	}
	ovExplodFlat, ovExplodNote, ovExplodPal = flat[:0], note[:0], pal[:0]
	return true
}

// ovPalWorker: the one goroutine (0..w-1) that steps PalFX pf in phase 3
func ovPalWorker(pf *PalFX, w int) int {
	h := uint64(uintptr(unsafe.Pointer(pf))>>4) * 0x9e3779b97f4a7c15
	return int((h >> 32) % uint64(w))
}

// ovParRun: f(0..workers-1), f(0) on this thread, the rest on goroutines; a panic in one is carried here
func ovParRun(workers int, f func(w int)) {
	if workers <= 1 {
		f(0)
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failed interface{}
	run := func(w int) {
		defer func() {
			if r := recover(); r != nil {
				mu.Lock()
				if failed == nil {
					failed = r
				}
				mu.Unlock()
			}
		}()
		f(w)
	}
	for w := 1; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			run(w)
		}(w)
	}
	run(0)
	wg.Wait()
	if failed != nil {
		panic(failed)
	}
}

// ovParCap: a scratch buffer's capacity for n (room to grow: explods pile up over a long match)
func ovParCap(n int) int { return n + n/4 + 64 }
