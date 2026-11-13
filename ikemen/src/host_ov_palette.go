package main

// A fighter's palette (the .def's palN, an ACT file) as its spawn picks it ("pal") and changed live (op "palette").
//
// The stock round start (system.go, roundsExisted == 0) puts the selected palette on an SFFv1 fighter by RemapPal
// 1,1 -> 1,palno and leaves an SFFv2 one alone, so an SFFv2 fighter whose .def lists ACT palettes is drawn in its first
// palette whatever was selected. Its ACTs overwrite its SFF's palettes 1,n at load (loadPalettes) and its sprites draw
// with 1,1, so the same RemapPal puts the selected palette on: done here when its palette is not the first (the first:
// untouched). Env IKEMEN_PAL_V2=0: the stock way (an SFFv2 fighter always in its first palette).

import (
	"os"
)

var ovPalV2 = os.Getenv("IKEMEN_PAL_V2") != "0"

// ovPaletteApply: its selected palette (gi.palno) on its sprites, from a clean remap of its palette list (its spawn:
// what the stock round start does; `live`: a change - its own remap thrown away first)
func ovPaletteApply(c *Char, live bool) {
	gi := c.gi()
	gi.palettedata.palList.ResetRemap()
	if live {
		c.remapPal(c.getPalfx(), [...]int32{-1, 0}, [...]int32{-1, 0}) // (clears its remap)
	}
	if gi.sff.header.Version[0] == 1 || ovPalV2 && gi.palno > 1 {
		c.remapPal(c.getPalfx(), [...]int32{1, 1}, [...]int32{1, gi.palno})
	}
	gi.remappedpal = [2]int32{1, gi.palno}
}

// ovPaletteValid: op "palette" {"h", "pal": 1..PaletteMax}
func ovPaletteValid(m map[string]interface{}) bool {
	v, ok := m["pal"].(float64)
	return ok && v == float64(int(v)) && v >= 1 && int(v) <= sys.cfg.Config.PaletteMax
}

// ovPaletteSet: op "palette" on the Ikemen thread - the fighter in slot pn in palette pal (one still loading takes it
// at its spawn); a palette its .def does not have is refused, the one it has kept
func ovPaletteSet(pn int, c *Char, s *ovSlot, pal int32) {
	if s == nil {
		return
	}
	if c == nil || !s.ready {
		s.pal = int(pal)
		ovEvent(`{"e":"palette","h":%d,"pal":%d,"ok":true,"at":"spawn"}`, pn, pal)
		return
	}
	gi := c.gi()
	if p, ok := gi.palInfo[int(pal-1)]; !ok || !p.exists {
		ovEvent(`{"e":"palette","h":%d,"pal":%d,"ok":false,"kept":%d}`, pn, pal, gi.palno)
		return
	}
	s.pal = int(pal)
	gi.palno = pal
	ovPaletteApply(c, true)
	ovEvent(`{"e":"palette","h":%d,"pal":%d,"ok":true,"at":"live"}`, pn, pal)
}
