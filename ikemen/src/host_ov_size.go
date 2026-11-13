package main

import "sync"

// A fighter bigger or smaller than it loaded. A fighter's [Size] xscale / yscale scale its drawing (Char.cueDraw's
// basescale * size.xscale), its hit and hurt boxes (updateClsnScale) and its projectiles (their scale times its own),
// so a fighter at its own scale times n is drawn, strikes and is struck as a bigger fighter is. The host sets n with
// the spawn ("size") or op "size" {"scale": n}; the slot keeps it (ovSlot.sizeMul) and ovCommitSpawns puts it on the
// fighter as it goes live.

// ovSizeBase: *Char -> its [Size] xscale, yscale as loaded (a second call never compounds)
var ovSizeBase sync.Map

// ovApplySize: c at its own [Size] scale times mul (0: none - as loaded)
func ovApplySize(c *Char, mul float32) {
	if c == nil || mul <= 0 {
		return
	}
	if c.size.xscale == 0 || c.size.yscale == 0 {
		return // (not loaded yet - an empty placeholder's zeros: never its base, or the fighter is drawn at no size at all)
	}
	v, _ := ovSizeBase.LoadOrStore(c, [2]float32{c.size.xscale, c.size.yscale})
	base := v.([2]float32)
	if base[0] == 0 || base[1] == 0 {
		base = [2]float32{c.size.xscale, c.size.yscale}
		ovSizeBase.Store(c, base)
	}
	c.size.xscale, c.size.yscale = base[0]*mul, base[1]*mul
	c.updateClsnScale()
	ovLog("size: %s x%.2f", c.name, mul)
}
