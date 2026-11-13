package main

// State dump for the parity test (tools/parity). This file uses nothing of the host layer, so the same file compiles
// into stock Ikemen GO; the test builds stock with it and compares the two dumps line by line.
//
//   IKEMEN_DUMP=<file>        write one line per logic tick, from the first tick that has two fighters loaded
//   IKEMEN_DUMP_SEED=<n>      the random seed set on that first tick (default 1)
//   IKEMEN_DUMP_TICKS=<n>     stop writing after n ticks (default 1200)
//   IKEMEN_DUMP_EXIT=1        exit the process when the dump is complete (the stock executable)
//
// A line holds the tick, the random seed before the tick runs, the round state, and for every character in run order:
// id, helper index, state, state time, animation, animation element, life, facing, position and velocity.

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
)

var hostDump struct {
	checked bool
	w       *bufio.Writer
	f       *os.File
	tick    int
	ticks   int
	exit    bool
}

func hostDumpTick() {
	d := &hostDump
	if !d.checked {
		d.checked = true
		path := os.Getenv("IKEMEN_DUMP")
		if path == "" {
			return
		}
		f, err := os.Create(path)
		if err != nil {
			return
		}
		d.f, d.w = f, bufio.NewWriterSize(f, 1<<16)
		d.ticks = 1200
		if n, err := strconv.Atoi(os.Getenv("IKEMEN_DUMP_TICKS")); err == nil && n > 0 {
			d.ticks = n
		}
		d.exit = os.Getenv("IKEMEN_DUMP_EXIT") == "1"
	}
	if d.w == nil {
		return
	}
	if d.tick == 0 {
		if len(sys.charList.runOrder) < 2 {
			return
		}
		seed := int32(1)
		if n, err := strconv.Atoi(os.Getenv("IKEMEN_DUMP_SEED")); err == nil && n > 0 {
			seed = int32(n)
		}
		sys.randseed = seed
	}
	fmt.Fprintf(d.w, "t %d seed %d rs %d", d.tick, sys.randseed, sys.roundState())
	for _, c := range sys.charList.runOrder {
		if c == nil {
			continue
		}
		elem := int32(-1)
		if c.anim != nil {
			elem = c.animElemNo(0).ToI()
		}
		fmt.Fprintf(d.w, " | %d %d st %d %d an %d %d life %d f %g p %.4f %.4f %.4f v %.4f %.4f %.4f",
			c.id, c.helperIndex, c.ss.no, c.ss.time, c.animNo, elem, c.life, c.facing,
			c.pos[0], c.pos[1], c.pos[2], c.vel[0], c.vel[1], c.vel[2])
	}
	d.w.WriteByte('\n')
	d.tick++
	if d.tick >= d.ticks {
		d.w.Flush()
		d.f.Close()
		d.w = nil
		if d.exit {
			os.Exit(0)
		}
	}
}
