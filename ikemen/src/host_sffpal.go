package main

import "io"

// SFFv1 linked sprites and the "same palette" flag.
//
// An SFFv1 subheader flags "same palette as the previous image". For a sprite with its own pixel data Ikemen reads
// it so (Sprite.read: prev's palette). A linked sprite (no data, a copy of another's pixels) instead took its link
// target's palette whatever the flag said. Recoloured sprite sets rely on the flag: a set links some of its sprites to
// another set's pixels, right after a sprite of its own palette, and expects them in its own colours. Rule: a linked
// character sprite flagged "same palette", right after a sprite of the same group that carries a palette of its own,
// takes that palette. Off in parity mode.

// ovPeekSamePal: the palette flag of the SFFv1 subheader just read (readHeader stops right before it)
func ovPeekSamePal(f io.ReadSeeker) bool {
	var b [1]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	f.Seek(-1, io.SeekCurrent)
	return b[0] != 0
}

// ovLinkedPal: applies the rule to linked sprite i (already a shareCopy of its link); true when it ends up with a
// palette of its own
// (off in the parity mode: the gate compares the engine with stock Ikemen, which draws these the old way)
func ovLinkedPal(list []*Sprite, ownPal []bool, i, link int, samePal bool) bool {
	if !ovOn || ovCfg.ParityMode {
		return false
	}
	if !samePal {
		return ownPal[link]
	}
	if i == 0 || !ownPal[i-1] || list[i-1] == nil || list[i-1].Group != list[i].Group {
		return false
	}
	p, s := list[i-1], list[i]
	s.Pal, s.palidx, s.sffv1BasePal = p.Pal, p.palidx, p.sffv1BasePal
	return true
}
