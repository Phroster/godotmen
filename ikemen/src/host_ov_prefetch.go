package main

// software prefetch for the Ikemen thread's walks over scattered heap objects - the
// between-ticks steps' walks over every explod (ovHoleStep, ovLiquidStep) and the draw capture's walk over every sprite
// (DrawList.draw). Late in a large fight these walk ~2,000 explods and ~3,000 sprites a tick, each a separate heap object (an
// Explod is ~1.2 KB, its lines far apart), and the walks waited on one cache miss after another (the profile's
// flat time sat on each object's first field read). Each walk now asks for the lines a few objects ahead
// (hostpf.Lines: PREFETCHT0), so the misses overlap. A prefetch is only a hint: it reads nothing into the program,
// writes nothing, never faults - the walks' results are those of the walks without it. The addresses come from plain
// reads of pointers the walks themselves read a few iterations later, on the same thread, with nothing else running.
// Env IKEMEN_PREFETCH=0: off (A/B).

import (
	"os"
	"unsafe"

	"github.com/ikemen-engine/Ikemen-GO/src/hostpf"
)

var ovPfOn = hostpf.On && os.Getenv("IKEMEN_PREFETCH") != "0"

// ovPfSet: the offsets of the lines to ask for in one kind of object (one per 64-byte line of the fields listed)
type ovPfSet struct {
	offs [24]uint16
	n    int
}

func (s *ovPfSet) add(off uintptr) {
	l := uint16(off &^ 63)
	for i := 0; i < s.n; i++ {
		if s.offs[i] == l {
			return
		}
	}
	if s.n < len(s.offs) {
		s.offs[s.n] = l
		s.n++
	}
}

// addAll: every line of an object of size n (bytes from 0)
func (s *ovPfSet) addAll(n uintptr) {
	for o := uintptr(0); o < n; o += 64 {
		s.add(o)
	}
}

func (s *ovPfSet) ask(p unsafe.Pointer) {
	if p != nil && s.n > 0 {
		hostpf.Lines(p, &s.offs[0], s.n)
	}
}

// The draw capture (DrawList.draw's walk over the sorted pass): what one sprite's draw reads - its SpriteData, its
// Animation and Sprite, its PalFX, its AIR frame, its root fighter's playerNo/localcoord and its explod's huge-rule fields
var (
	ovPfSprite = func() (s ovPfSet) { s.addAll(unsafe.Sizeof(SpriteData{})); return }()
	ovPfAnim   = func() (s ovPfSet) { s.addAll(unsafe.Sizeof(Animation{})); return }()
	ovPfSpr    = func() (s ovPfSet) { s.addAll(unsafe.Sizeof(Sprite{})); return }()
	ovPfPalFX  = func() (s ovPfSet) {
		var p PalFX
		s.add(unsafe.Offsetof(p.remap))
		s.add(unsafe.Offsetof(p.enable))
		s.add(unsafe.Offsetof(p.eiTime))
		s.add(0)
		return
	}()
	ovPfRoot = func() (s ovPfSet) {
		var c Char
		s.add(unsafe.Offsetof(c.playerNo))
		s.add(unsafe.Offsetof(c.localcoord))
		return
	}()
	ovPfExplodDraw = func() (s ovPfSet) {
		var e Explod
		s.add(unsafe.Offsetof(e.ovSclMax))
		s.add(unsafe.Offsetof(e.id))
		s.add(unsafe.Offsetof(e.timestamp))
		return
	}()
)

// ovPfDrawAhead: (DrawList.draw at position n of the sorted pass) stages a few sprites ahead, each asking for what the
// stage before it brought in: the SpriteData 8 ahead; the Animation, PalFX, root and explod it points to 5 ahead; the
// Sprite, AIR frame and palette list the Animation points to 3 ahead; the palette map's entry 2 ahead; the palette's
// slice header and palette texture entry 1 ahead (Animation.pal / Sprite.GetPal / GetPalTex's three lookups)
func ovPfDrawAhead(dl DrawList, list []int, n int) {
	if j := n + 8; j < len(list) {
		ovPfSprite.ask(unsafe.Pointer(dl[list[j]]))
	}
	if j := n + 5; j < len(list) {
		if s := dl[list[j]]; s != nil {
			ovPfAnim.ask(unsafe.Pointer(s.anim))
			ovPfPalFX.ask(unsafe.Pointer(s.pfx))
			ovPfRoot.ask(unsafe.Pointer(s.ovRoot))
			ovPfExplodDraw.ask(unsafe.Pointer(s.ovExplod))
		}
	}
	if j := n + 3; j < len(list) {
		if s := dl[list[j]]; s != nil {
			if a := s.anim; a != nil {
				ovPfSpr.ask(unsafe.Pointer(a.spr))
				var frame unsafe.Pointer
				if k := int(a.drawidx); k >= 0 && k < len(a.frames) {
					frame = unsafe.Pointer(&a.frames[k])
				}
				pl := a.palettedata
				if pl == nil && a.sff != nil {
					pl = &a.sff.palList
				}
				hostpf.T0x4(frame, unsafe.Pointer(pl), nil, nil)
			}
		}
	}
	if j := n + 2; j < len(list) {
		if pm, k := ovPfPalMap(dl[list[j]]); k >= 0 {
			hostpf.T0x4(unsafe.Pointer(&pm[k]), nil, nil, nil)
		}
	}
	if j := n + 1; j < len(list) {
		s := dl[list[j]]
		if pm, k := ovPfPalMap(s); k >= 0 {
			pl, m := ovPfPalList(s), pm[k]
			var a, b unsafe.Pointer
			if m >= 0 && m < len(pl.palettes) {
				a = unsafe.Pointer(&pl.palettes[m])
			}
			if m >= 0 && m < len(pl.PalTex) {
				b = unsafe.Pointer(&pl.PalTex[m])
			}
			hostpf.T0x4(a, b, nil, nil)
		}
	}
}

// ovPfPalList: the palette list Animation.pal reads for s (nil: none to look up - its sprite has its own palette or
// none); reads only
func ovPfPalList(s *SpriteData) *PaletteList {
	if s == nil || s.anim == nil || s.anim.spr == nil {
		return nil
	}
	a := s.anim
	if len(a.spr.Pal) > 0 || a.spr.coldepth > 8 {
		return nil // (GetPal returns the sprite's own palette)
	}
	if a.palettedata != nil {
		return a.palettedata
	}
	if a.sff != nil {
		return &a.sff.palList
	}
	return nil
}

// ovPfPalMap: the logical palette map Animation.pal's lookups index - the list's own, or the PalFX remap it swaps in
// (SwapPalMap: only of the same length) - and the index they take (GetPal / GetPalTex: the sprite's, or 0 out of range);
// k < 0: nothing to look up. Reads only.
func ovPfPalMap(s *SpriteData) (pm []int, k int) {
	pl := ovPfPalList(s)
	if pl == nil {
		return nil, -1
	}
	pm = pl.paletteMap
	if p := s.pfx; p != nil && len(p.remap) > 0 && len(p.remap) == len(pm) {
		pm = p.remap
	}
	if len(pm) == 0 {
		return nil, -1
	}
	k = int(s.anim.spr.palidx)
	if k < 0 || k >= len(pm) {
		k = 0
	}
	return pm, k
}

// The explod update and cue (System.explodUpdate, explodCueDraw: Explod.update, ovCueExplod, Explod.cueDraw) read
// nearly every line of an explod, then its Animation and PalFX: every line of the explod 6 ahead, the objects it points
// to 3 ahead (its lines asked for 3 steps before)
var (
	ovPfExplodAll = func() (s ovPfSet) { s.addAll(unsafe.Sizeof(Explod{})); return }()
	ovPfPalFXAll  = func() (s ovPfSet) { s.addAll(unsafe.Sizeof(PalFX{})); return }()
)

// ovPfExplodsWhole: (an explod update or cue walk at index i of list) the explod 6 ahead whole, the anim and PalFX of the
// one 3 ahead
func ovPfExplodsWhole(list []*Explod, i int) {
	if j := i + 6; j < len(list) {
		ovPfExplodAll.ask(unsafe.Pointer(list[j]))
	}
	if j := i + 3; j < len(list) {
		if e := list[j]; e != nil {
			ovPfAnim.ask(unsafe.Pointer(e.anim))
			ovPfPalFXAll.ask(unsafe.Pointer(e.palfx))
		}
	}
}

// The explod update (Explod.update on an explod at rest) reads about half of an explod's cache lines, then its
// Animation (UpdateSprite) and, with its own palette, its PalFX (measured: the whole explod asked for cost the update
// walk as much as it saved on a quiet machine)
var ovPfExplodUpd = func() (s ovPfSet) {
	var e Explod
	for _, o := range []uintptr{unsafe.Offsetof(e.id),
		unsafe.Offsetof(e.time), unsafe.Offsetof(e.pos), unsafe.Offsetof(e.removetime), unsafe.Offsetof(e.velocity),
		unsafe.Offsetof(e.accel), unsafe.Offsetof(e.anim), unsafe.Offsetof(e.supermovetime), unsafe.Offsetof(e.animelem),
		unsafe.Offsetof(e.ownpal), unsafe.Offsetof(e.oldPos), unsafe.Offsetof(e.interPos), unsafe.Offsetof(e.palfx),
		unsafe.Offsetof(e.syncId), unsafe.Offsetof(e.localscl), unsafe.Offsetof(e.interpolate),
		unsafe.Offsetof(e.interpolate_animelem), unsafe.Offsetof(e.interpolate_pos), unsafe.Offsetof(e.pauseBool),
		unsafe.Offsetof(e.customShader)} {
		s.add(o)
	}
	return
}()

// ovPfExplodsUpdate: (System.explodUpdate at index i of list) the update's lines of the explod 6 ahead; the Animation
// - and the PalFX if it steps one - of the explod 3 ahead
func ovPfExplodsUpdate(list []*Explod, i int) {
	if j := i + 6; j < len(list) {
		ovPfExplodUpd.ask(unsafe.Pointer(list[j]))
	}
	if j := i + 3; j < len(list) {
		if e := list[j]; e != nil {
			ovPfAnim.ask(unsafe.Pointer(e.anim))
			if e.ownpal {
				ovPfPalFXAll.ask(unsafe.Pointer(e.palfx))
			}
		}
	}
}
