package main

// The explods' parallel cue. With thousands of explods alive, System.explodCueDraw's walk over them on the engine
// thread every frame - its sort keys, ovCueExplod (the explod's depth and anchor for its card) and Explod.cueDraw (its
// SpriteData) - is mostly first-touch cache misses on the explods, their animations, sprites and makers. Each
// explod's part of that is a function of the explod, its maker chars and the frame's globals, writing nothing but the
// explod itself, so it is worked out on a few goroutines; the engine thread then lists the sprites in draw order
// exactly as the walk did.
//
//   - Phase 1 (workers, storage order, any thread): e.sortindex = its storage position and its draw key (explodDrawKey,
//     the walk's own), then - unless e is a fallback (below) - ovCueExplodTo (the values ovCueExplod leaves in the
//     cue's globals, into an ovCueVals of its own), Explod.cueSprite (its SpriteData, made from those values instead of
//     the globals: newSpriteDataOf) and cueEmitPrep. What these write: e's own fields (sortindex; Interpolate's
//     interpolation state - only ever of the explod being cued) and fresh allocations. What they read: e, its
//     animation and sprite, its maker and root chars, the frozen id index, the fight FX list, the slots, the camera -
//     none of it written while the explods are cued (the walk itself wrote only the explods and the draw lists; no
//     char is added or removed: ovFreezeIDs; no fight FX loads: ovFreezeFFX). So each explod's values and sprite are
//     what the walk made of it, whatever order the workers take them in.
//   - Then the engine thread sorts the keys (unique - one order) and, in that order, does the walk's writes to shared
//     lists: the afterimage (AfterImage.recAndCue lists its sprites and swaps palette maps), DrawList.add, the shadow and
//     reflection lists. The cue's globals are set from an explod's values only where something reads them: before a
//     fallback explod is cued the old way, so the globals it sees are what the walk left there.
//   - Fallback (cued serially in its place, the old way: ovCueExplod + cueDraw): an explod with a maker slot whose root
//     is missing (ovCueMakerGone: its cue keeps the depth of the sprite cued before it).
//
// Off under IKEMEN_LAZY_SFF=1 (a fight-screen sprite's lazy decode on the host thread) and under IKEMEN_CUE_PAR=0.

import (
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

// ovCueVals: the cue's values for one owner - what newSpriteData copies into each of its sprites
type ovCueVals struct {
	Z, X, Y, AnchorY float32
	Root             *Char
	Kind             int8
	AnimNo           int32
	ExplodOf         *Explod
	RootZ            float32
}

// set: v into the cue's globals
func (v *ovCueVals) set() {
	ovCueZ, ovCueX, ovCueY, ovCueAnchorY = v.Z, v.X, v.Y, v.AnchorY
	ovCueRoot, ovCueKind, ovCueAnimNo, ovCueExplodOf = v.Root, v.Kind, v.AnimNo, v.ExplodOf
	ovCueRootZ = v.RootZ
}

// get: the cue's globals into v
func (v *ovCueVals) get() {
	v.Z, v.X, v.Y, v.AnchorY = ovCueZ, ovCueX, ovCueY, ovCueAnchorY
	v.Root, v.Kind, v.AnimNo, v.ExplodOf = ovCueRoot, ovCueKind, ovCueAnimNo, ovCueExplodOf
	v.RootZ = ovCueRootZ
}

// newSpriteDataOf: newSpriteData with the cue's values v instead of the globals (field for field the same)
func newSpriteDataOf(v *ovCueVals) *SpriteData { return newSpriteDataAt(nil, v) }

// newSpriteDataAt: newSpriteDataOf made in dst (every field written; nil: a new allocation)
func newSpriteDataAt(dst *SpriteData, v *ovCueVals) *SpriteData {
	if dst == nil {
		dst = new(SpriteData)
	}
	*dst = SpriteData{
		trans:        TT_default,
		alpha:        [2]int32{255, 0},
		airOffsetFix: [2]float32{1, 1},
		ovZ:          v.Z,
		ovX:          v.X,
		ovY:          v.Y,
		ovAnchorY:    v.AnchorY,
		ovRoot:       v.Root,
		ovKind:       v.Kind,
		ovAnimNo:     v.AnimNo,
		ovExplod:     v.ExplodOf,
		ovRootZ:      v.RootZ,
	}
	return dst
}

var ovCueParOn = os.Getenv("IKEMEN_CUE_PAR") != "0"

const ovCueParBlock = 32

// ovCuePrep: one explod's part of the cue, worked out on a worker
type ovCuePrep struct {
	v    ovCueVals
	sd   *SpriteData // nil: not drawn (a gate of cueSprite)
	emit explodEmit
	mode uint8 // ovCuePrepped, or ovCueFallback (cued the old way in its place)
}

const (
	ovCuePrepped  = 1
	ovCueFallback = 2
)

var ovCuePreps []ovCuePrep
var ovCueKeysIn, ovCueKeysOut []uint64 // (the last frame's keys before and after the sort)
var ovCueList []*Explod

// explodCueDrawPar: System.explodCueDraw for the overlay, in parallel; false: not taken (the walk runs)
func (s *System) explodCueDrawPar() bool {
	if !ovOn || !ovCueParOn || ovLazyAll {
		return false
	}
	n := 0
	for i := range s.explods {
		n += len(s.explods[i])
	}
	if n < ovExplodParMin || n > 0x7fffffff {
		return false
	}
	flat := ovCueList[:0]
	for i := range s.explods {
		flat = append(flat, s.explods[i]...)
	}
	if cap(ovCuePreps) < n {
		ovCuePreps = make([]ovCuePrep, ovParCap(n))
	}
	preps := ovCuePreps[:n]
	if cap(explodDrawKeys) < n {
		explodDrawKeys = make([]uint64, ovParCap(n))
	}
	keys := explodDrawKeys[:n]
	// Phase 1: sort keys, values and sprites, on up to ovExplodParMax goroutines
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
				next.Store(int64(n))
			}
		}()
		var block []SpriteData // (the sprites are made in blocks: one allocation per ovCueParBlock instead of one each)
		for {
			lo := int(next.Add(ovCueParBlock)) - ovCueParBlock
			if lo >= n {
				if ovSdPoolOn {
					ovSdClearTail(block) // (this worker's last block's unused tail - host_ov_sdpool.go)
				}
				return
			}
			hi := Min(lo+ovCueParBlock, n)
			for k := lo; k < hi; k++ {
				if len(block) == 0 {
					block = ovSdBlock() // (this cue's storage from the pool - host_ov_sdpool.go)
				}
				e := flat[k]
				e.sortindex = k // (explodCueDraw's: unique reference for this frame)
				keys[k] = explodDrawKey(e)
				p := &preps[k]
				if ovCueMakerGone(e) {
					p.mode, p.sd = ovCueFallback, nil
					continue
				}
				p.v = ovCueVals{}
				ovCueExplodTo(&p.v, e)
				p.sd = e.cueSprite(&p.v, &block[0])
				if p.sd != nil {
					block = block[1:]
					p.emit = e.cueEmitPrep()
					p.emit.known, p.emit.blank = true, p.sd.isBlank()
				}
				p.mode = ovCuePrepped
			}
		}
	}
	workers := Min(ovExplodParMax, runtime.GOMAXPROCS(0))
	if b := (n + ovCueParBlock - 1) / ovCueParBlock; b < workers {
		workers = b
	}
	var wg sync.WaitGroup
	for w := 1; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			work()
		}()
	}
	work()
	wg.Wait()
	if failed != nil {
		panic(failed)
	}

	// The draw order (explodDrawSort's, from the keys: unique, so one order). Most frames of a long match list the same
	// explods in the same places as the last one: the same keys sort the same, so the last sorted keys are reused
	if slices.Equal(keys, ovCueKeysIn) {
		copy(keys, ovCueKeysOut)
	} else {
		ovCueKeysIn = append(ovCueKeysIn[:0], keys...)
		slices.Sort(keys)
		ovCueKeysOut = append(ovCueKeysOut[:0], keys...)
	}
	order := s.explodDrawOrder[:0]
	for _, key := range keys {
		order = append(order, flat[explodDrawIdx(key)])
	}
	s.explodDrawOrder = order

	// Phase 2: in draw order, the walk's writes to the shared lists
	prev := -1 // (the storage index of the explod listed last: its values are the cue's globals now)
	for _, key := range keys {
		k := explodDrawIdx(key)
		e, p := flat[k], &preps[k]
		if p.mode == ovCueFallback {
			if prev >= 0 && preps[prev].mode == ovCuePrepped {
				preps[prev].v.set()
			}
			ovCueExplod(e)
			e.cueDraw()
			prev = k
			continue
		}
		if p.sd != nil {
			e.cueEmit(p.sd, &p.emit)
		}
		prev = k
	}
	if prev >= 0 && preps[prev].mode == ovCuePrepped {
		preps[prev].v.set() // (the globals as the walk left them - ovCueEnd clears them next)
	}

	for i := range preps {
		preps[i].sd, preps[i].v.Root, preps[i].v.ExplodOf = nil, nil, nil
	}
	for i := range flat {
		flat[i] = nil
	}
	ovCueList, explodDrawKeys = flat[:0], keys[:0]
	return true
}

// The chars' cue values (System.cueDraw): each char's ovCueCharTo (own) - the values ovCueChar leaves in the cue's
// globals - worked out on a few goroutines before the cue walks the run order, which then sets the globals from them
// before each char's cueDraw. ovCueCharTo writes nothing global and reads the char, its root and the slots - none of
// which Char.cueDraw changes (it writes the char's own PalFX if it has none, its afterimage and the draw lists) - so
// each char's values are those ovCueChar gives.
var ovCharCues []ovCueVals
var ovCharCueOK []bool

func ovPrefetchCharCues(list []*Char) bool {
	n := len(list)
	if !ovCueParOn || n < ovCharParMin {
		return false
	}
	if cap(ovCharCues) < n {
		ovCharCues, ovCharCueOK = make([]ovCueVals, ovParCap(n)), make([]bool, ovParCap(n))
	}
	vals, ok := ovCharCues[:n], ovCharCueOK[:n]
	workers := Min(ovExplodParMax, runtime.GOMAXPROCS(0))
	chunk := (n + workers - 1) / workers
	ovParRun(workers, func(w int) {
		for i := w * chunk; i < Min((w+1)*chunk, n); i++ {
			if c := list[i]; c != nil {
				ovCueCharTo(&vals[i], c, true)
				ok[i] = true
			} else {
				ok[i] = false
			}
		}
	})
	ovCharCues, ovCharCueOK = vals, ok
	return true
}

// addKnown: DrawList.add with the sprite's isBlank already worked out (the parallel cue's workers: what it reads - the
// sprite's scale, its animation's scale and picture, the picture's texture fields - is not changed while sprites are
// cued; a fighter's or fight fx's picture is decoded at load, never on the host thread: ovLazySff)
func (dl *DrawList) addKnown(sd *SpriteData, blank bool) {
	if sys.frameSkip || sd == nil || blank {
		return
	}
	sd.sortindex = len(*dl)
	*dl = append(*dl, sd)
}
