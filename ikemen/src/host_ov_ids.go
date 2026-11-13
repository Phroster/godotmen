package main

import (
	"cmp"
	"slices"
)

// The frozen id index: System.playerID is a map lookup (charList.idMap) and the serial passes
// after the state pass ask it 2-3 times for every explod of the fight each tick (Explod.parent in Explod.cueDraw and
// ovCueExplod, binds in Explod.update) - ~2000 map lookups. While the cue, the explod update and the snapshot capture
// run, no char is added or removed (idMap changes only in CharList.add/delete/clear), so for their length idMap is
// copied into a dense slice by id and playerID reads that: the same *Char for every id, the same nil for every absent
// one. Engine thread only; the state pass's workers never run during a frozen pass (ovIDFrozen is false whenever they
// do). Ids at or above ovIDIndexMax keep the map lookup (the index is not built).

const ovIDIndexMax = 1 << 22 // (4 M ids: a 32 MB index at most - a 101-fighter fight uses tens of thousands)

var ovIDIndex []*Char
var ovIDTouched []int32
var ovIDFrozen bool

// ovFreezeIDs: idMap into the index until ovThawIDs; false (nothing frozen) when already frozen or an id is too big
func ovFreezeIDs() bool {
	if ovIDFrozen {
		return false
	}
	touched := ovIDTouched[:0]
	for id, c := range sys.charList.idMap {
		if id < 0 {
			continue // (playerID answers nil below 0 before any lookup)
		}
		if id >= ovIDIndexMax {
			for _, k := range touched {
				ovIDIndex[k] = nil
			}
			ovIDTouched = touched[:0]
			return false
		}
		if int(id) >= len(ovIDIndex) {
			n := Max(int(id)+1, 2*len(ovIDIndex))
			grown := make([]*Char, n)
			copy(grown, ovIDIndex)
			ovIDIndex = grown
		}
		ovIDIndex[id] = c
		touched = append(touched, id)
	}
	ovIDTouched = touched
	ovIDFrozen = true
	return true
}

func ovThawIDs() {
	for _, k := range ovIDTouched {
		ovIDIndex[k] = nil
	}
	ovIDTouched = ovIDTouched[:0]
	ovIDFrozen = false
}

// ovFrozenID: idMap[id] from the index (frozen only; id >= 0)
func ovFrozenID(id int32) (*Char, bool) {
	if int(id) >= len(ovIDIndex) {
		return nil, false
	}
	c := ovIDIndex[id]
	return c, c != nil
}

// The common FX SFFs (Animation.isCommonFX ranges over the sys.ffx map for every explod with a PalFX it cues): while
// the cue runs no fight FX loads on this thread, so the map's SFFs are listed once and isCommonFX scans that list -
// "some fight FX has this SFF", the same answer.
var ovFFXSffs []*Sff
var ovFFXFrozen bool

func ovFreezeFFX() bool {
	if ovFFXFrozen {
		return false
	}
	list := ovFFXSffs[:0]
	for _, fx := range sys.ffx {
		if fx != nil {
			list = append(list, fx.sff)
		}
	}
	ovFFXSffs, ovFFXFrozen = list, true
	return true
}

func ovThawFFX() {
	for i := range ovFFXSffs {
		ovFFXSffs[i] = nil
	}
	ovFFXSffs, ovFFXFrozen = ovFFXSffs[:0], false
}

// Priority sorts (CharList.updateRunOrder, collisionDetection): Ikemen sorts its chars with a comparator that asks
// each char's priority again at every comparison. Its order - priority descending, then id ascending - is a strict
// total order while every id is distinct, so it is unique: charPrioritySort puts it together from one 64-bit key per
// entry. Two equal keys (two chars with one id) hand the list, untouched, to the stock sort instead.
type charPrioKey struct {
	k   uint64
	idx int
}

var charPrioBuf []charPrioKey

func charPrioKeyOf(pri int, id int32) uint64 {
	return uint64(int64(1<<20)-int64(pri))<<32 | uint64(uint32(id)^0x80000000)
}

// charPrioritySort: order (n entries) sorted by key(i) (charPrioKeyOf of entry i); stock when two keys tie. move(perm)
// applies the sorted permutation: perm[n] = the old position now at n.
func charPrioritySort(n int, key func(i int) uint64, move func(perm []charPrioKey), stock func()) {
	buf := charPrioBuf[:0]
	for i := 0; i < n; i++ {
		buf = append(buf, charPrioKey{key(i), i})
	}
	slices.SortFunc(buf, func(a, b charPrioKey) int { return cmp.Compare(a.k, b.k) })
	charPrioBuf = buf[:0]
	for i := 1; i < len(buf); i++ {
		if buf[i].k == buf[i-1].k {
			stock()
			return
		}
	}
	move(buf)
}
