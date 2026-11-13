package main

import (
	"os"
	"sync/atomic"
)

// Per-player view of CharList.creationOrder.
//
// helper(id), IsHelper(id, index), helperindex and friends scan the whole
// creationOrder (every character of every player, 600+ late in a large fight) and
// keep only one player's helpers. The view holds, for each player number,
// that player's characters in creationOrder order - the same subsequence
// the scan visits - so the callers loop over a handful of entries and still
// apply every one of their original live checks (playerNo, helperIndex,
// destroy flag, helper ID) to each entry, in the same order.
//
// The view is rebuilt whenever it may differ from creationOrder: on every
// add/delete (mods), when creationOrder is a different slice (snapshot
// restore, clear) and when a slot remap rewrites player numbers. During the
// FFA state pass creationOrder only changes on the engine lane while every
// worker is parked; a worker that finds the view stale builds its own copy
// from the unchanging list (equal copies, published atomically).
type simCharView struct {
	base  **Char
	n     int
	mods  uint64
	lists [][]*Char
}

var simCharViewOn = os.Getenv("IKEMEN_SIM_CHARVIEW") != "0"

type simCharViewPtr = atomic.Pointer[simCharView]

func (cl *CharList) playerChars(pn int) []*Char {
	v := cl.view.Load()
	if v == nil || v.mods != atomic.LoadUint64(&simCharMods) || v.n != len(cl.creationOrder) ||
		(v.n > 0 && v.base != &cl.creationOrder[0]) {
		v = cl.buildView()
		cl.view.Store(v)
	}
	if pn < 0 || pn >= len(v.lists) {
		return nil
	}
	return v.lists[pn]
}

func (cl *CharList) buildView() *simCharView {
	v := &simCharView{n: len(cl.creationOrder), mods: atomic.LoadUint64(&simCharMods)}
	if v.n > 0 {
		v.base = &cl.creationOrder[0]
	}
	maxPn := -1
	for _, c := range cl.creationOrder {
		if c != nil && c.playerNo > maxPn {
			maxPn = c.playerNo
		}
	}
	if maxPn >= 0 {
		counts := make([]int, maxPn+1)
		for _, c := range cl.creationOrder {
			if c != nil && c.playerNo >= 0 {
				counts[c.playerNo]++
			}
		}
		all := make([]*Char, 0, v.n)
		v.lists = make([][]*Char, maxPn+1)
		for pn, n := range counts {
			v.lists[pn] = all[len(all) : len(all) : len(all)+n]
			all = all[:len(all)+n]
		}
		for _, c := range cl.creationOrder {
			if c != nil && c.playerNo >= 0 {
				v.lists[c.playerNo] = append(v.lists[c.playerNo], c)
			}
		}
	}
	return v
}

// creationOrder entries a per-player scan must visit: the player's view, or
// the whole list (IKEMEN_SIM_CHARVIEW=0; negative player numbers).
func (cl *CharList) helperScan(pn int) []*Char {
	if !simCharViewOn || pn < 0 {
		return cl.creationOrder
	}
	return cl.playerChars(pn)
}

// One process-wide counter, never copied or restored with a CharList: a view
// built before any later change (of any list, or a slot remap) never matches.
var simCharMods uint64

func (cl *CharList) touch() {
	atomic.AddUint64(&simCharMods, 1)
}
