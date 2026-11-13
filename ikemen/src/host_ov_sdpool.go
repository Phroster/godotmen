package main

// The cue's SpriteData storage, reused from one cue to the next. Every frame the cue makes a SpriteData per drawn
// char, explod and projectile - thousands a frame with many fighters, and as fresh allocations the biggest share of
// the garbage the Go GC's cycles are paced by. They come from blocks kept here, handed out again from the start at
// every System.cueDraw.
//
// Why that is the same work: (1) contents - each sprite is still written whole before use (newSpriteData assigns its
// complete composite literal, newSpriteDataAt likewise), so a reused
// one holds exactly what a fresh one would. (2) lifetime - a cue's sprites are referenced only by that cue's draw
// lists (sys.spriteList, and the shadow and reflection sprites of sys.shadowList / sys.reflectionList), the explods'
// parallel cue's preps (rewritten before they are read) and locals; everything that keeps a sprite past its frame
// copies it by value (AfterImage.recAfterImg `*img = *sd`, the replay's entries `e.data = *sd`, the draw snapshot's
// resolved RenderParams). Those lists are read by the frame's own draw capture (the Ikemen thread, before the cue
// returns to its loop) and by the replay recorder at the join (ovStep / ovCompleteStep: the Ikemen thread is parked
// until the next tick begins), and they are emptied by the next cue's clearSpriteData - right where the blocks are
// handed out again (System.cueDraw). Nothing keys on a sprite's address (no map or identity of *SpriteData; the
// replay's identity is the animation's address). (3) threads - System.cueDraw and newSpriteData run on the Ikemen
// thread (newSpriteData reads the cue's globals, which only that thread sets); the parallel explod cue's workers take
// whole blocks under the mutex while the Ikemen thread waits for them. IKEMEN_SD_POOL=0: fresh allocations as before
// (A/B).
//
// GC liveness: blocks a cue did not reach are cleared at the next reset (their sprites are at least a frame old), and
// so is the unused tail of every block it reached last (the serial block and the frame pools' current block at the
// reset, a parallel cue worker's last block when it is done - ovSdClearTail), so the pool keeps nothing alive longer
// than one frame past its use, as the garbage did until the GC came by.

import (
	"os"
	"sync"
)

var ovSdPoolOn = os.Getenv("IKEMEN_SD_POOL") != "0"

var ovSdPool struct {
	mu     sync.Mutex
	blocks [][]SpriteData
	used   int          // blocks handed out since the last reset
	dirty  int          // leading blocks that may still hold sprites of an earlier cue
	serial []SpriteData // (Ikemen thread) the rest of newSpriteData's current block
}

// ovSdPoolReset: (Ikemen thread, System.cueDraw right after clearSpriteData) the last cue's sprites are free again
func ovSdPoolReset() {
	p := &ovSdPool
	p.mu.Lock()
	if p.used > p.dirty {
		p.dirty = p.used
	}
	for i := p.used; i < p.dirty; i++ { // (reached by an earlier cue but not the last: their references dropped)
		b := p.blocks[i]
		for j := range b {
			b[j] = SpriteData{}
		}
	}
	p.dirty = p.used
	p.used = 0
	ovSdClearTail(p.serial) // (the serial block's unused tail: stale sprites of earlier cues)
	p.serial = nil
	p.mu.Unlock()
	ovShadowPool.reset()
	ovReflectionPool.reset()
}

// The cue's shadow and reflection sprites (newShadowSprite, newReflectionSprite: the Ikemen thread only - the char,
// projectile and serial explod cues and the parallel explod cue's emission phase), the same way: written whole by
// their constructors, referenced only by this cue's sys.shadowList / sys.reflectionList and locals, handed out again
// at the next System.cueDraw (ovSdPoolReset)
var ovShadowPool ovFramePool[ShadowSprite]
var ovReflectionPool ovFramePool[ReflectionSprite]

// ovFramePool: one thread's per-cue storage (see ovSdPool)
type ovFramePool[T any] struct {
	blocks [][]T
	used   int // blocks handed out since the last reset
	dirty  int // leading blocks that may still hold values of an earlier cue
	cur    []T // the rest of the current block
}

func (p *ovFramePool[T]) take() *T {
	if !ovSdPoolOn {
		return new(T)
	}
	if len(p.cur) == 0 {
		if p.used == len(p.blocks) {
			p.blocks = append(p.blocks, make([]T, 64))
		}
		p.cur = p.blocks[p.used]
		p.used++
		if p.used > p.dirty {
			p.dirty = p.used
		}
	}
	t := &p.cur[0]
	p.cur = p.cur[1:]
	return t
}

func (p *ovFramePool[T]) reset() {
	var zero T
	for j := range p.cur { // (the current block's unused tail)
		p.cur[j] = zero
	}
	for i := p.used; i < p.dirty; i++ { // (reached by an earlier cue but not the last: their references dropped)
		b := p.blocks[i]
		for j := range b {
			b[j] = zero
		}
	}
	p.dirty = p.used
	p.used = 0
	p.cur = nil
}

// ovSdBlock: a block of ovCueParBlock sprites, free until the next ovSdPoolReset (any thread)
func ovSdBlock() []SpriteData {
	if !ovSdPoolOn {
		return make([]SpriteData, ovCueParBlock)
	}
	p := &ovSdPool
	p.mu.Lock()
	if p.used == len(p.blocks) {
		p.blocks = append(p.blocks, make([]SpriteData, ovCueParBlock))
	}
	b := p.blocks[p.used]
	p.used++
	if p.used > p.dirty {
		p.dirty = p.used
	}
	p.mu.Unlock()
	return b
}

// ovSdClearTail: the unused rest of a block, cleared (memory only: nothing reads an unused slot, but a stale sprite
// there keeps its animation, char and textures alive)
func ovSdClearTail(tail []SpriteData) {
	for i := range tail {
		tail[i] = SpriteData{}
	}
}

// ovSdTake: (Ikemen thread) storage for one sprite of this cue - newSpriteData writes all of it
func ovSdTake() *SpriteData {
	if !ovSdPoolOn {
		return new(SpriteData)
	}
	p := &ovSdPool
	if len(p.serial) == 0 {
		p.serial = ovSdBlock()
	}
	sd := &p.serial[0]
	p.serial = p.serial[1:]
	return sd
}
