package main

// Dev diagnostic, env IKEMEN_TICK_LOG=<file>: a timeline of every Ikemen tick and every host call
// that can hold up a Godot frame - step_begin, step_end (the join), render - in QPC microseconds (ovTimingUs), one
// tab-separated line each, for attributing the long frames of a large fight. Lines:
//
//	T tick wake between m0..m9 publish done gc_cycles speaker_wait_us speaker_locks   (Ikemen thread, each tick)
//	B qpc ticks                                                                        (step_begin: the wake sent)
//	E enter recv capture_end exit ticks                                                (step_end: the join)
//	R enter drained exit ticks                                                         (render: GL tasks, the draw)
//	A enter exit frames                                                                (the host's audio mix callback)
//
// m0..m4: CharList.action's phases (simProfilePhase: commands start/end, prepare end, states end, finish end); m5:
// charUpdate end; m6: collisions + globalTick end; m7: explodUpdate end; m8: the loop's cueDraw start; m9: its end.
// Env IKEMEN_ALLOC_PPROF=<file> (with it): the allocation profile at tick IKEMEN_ALLOC_PPROF_AT (4000).
// Unset, ovTickLog is nil: nothing here runs and no simulation state is read or written either way.
import (
	"os"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type ovTickLogT struct {
	mu   sync.Mutex
	buf  []byte
	f    *os.File
	gc   []metrics.Sample // (Ikemen thread only)
	busy []byte
}

var ovTickLog = func() *ovTickLogT {
	p := os.Getenv("IKEMEN_TICK_LOG")
	if p == "" {
		return nil
	}
	f, err := os.Create(p)
	if err != nil {
		return nil
	}
	l := &ovTickLogT{f: f, buf: make([]byte, 0, 1<<20), busy: make([]byte, 0, 1<<20),
		gc: []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}}}
	allocs, allocsAt := os.Getenv("IKEMEN_ALLOC_PPROF"), uint64(4000) // (dev: the allocation profile so far, at that tick)
	if v, err := strconv.Atoi(os.Getenv("IKEMEN_ALLOC_PPROF_AT")); err == nil && v > 0 {
		allocsAt = uint64(v)
	}
	// (dev, IKEMEN_ALLOC_PPROF_FROM=<tick> also writes the profile at that earlier tick to
	// <file>.from - `pprof -diff_base <file>.from <file>` is the fight's window alone - and <file>.metrics gets the
	// runtime's allocation / GC counters at both points: bytes allocated per tick and GC cycles in the window)
	allocsFrom := uint64(0)
	if v, err := strconv.Atoi(os.Getenv("IKEMEN_ALLOC_PPROF_FROM")); err == nil && v > 0 {
		allocsFrom = uint64(v)
	}
	allocMetrics := func(tag string) {
		s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}, {Name: "/gc/heap/allocs:objects"},
			{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/cpu/classes/gc/mark/assist:cpu-seconds"},
			{Name: "/cpu/classes/gc/mark/dedicated:cpu-seconds"}, {Name: "/cpu/classes/gc/mark/idle:cpu-seconds"},
			{Name: "/gc/heap/live:bytes"}, {Name: "/gc/heap/goal:bytes"},
			// (what the mark phase scans - pointer-bearing heap, globals, stacks - against the live heap)
			{Name: "/gc/scan/heap:bytes"}, {Name: "/gc/scan/globals:bytes"}, {Name: "/gc/scan/stack:bytes"},
			{Name: "/gc/heap/objects:objects"}, {Name: "/memory/classes/heap/objects:bytes"}}
		metrics.Read(s)
		line := tag + "\ttick=" + strconv.FormatUint(atomic.LoadUint64(&ovTicks), 10)
		for _, m := range s {
			line += "\t" + m.Name + "="
			switch m.Value.Kind() {
			case metrics.KindUint64:
				line += strconv.FormatUint(m.Value.Uint64(), 10)
			case metrics.KindFloat64:
				line += strconv.FormatFloat(m.Value.Float64(), 'f', 4, 64)
			default:
				line += "?"
			}
		}
		if f, err := os.OpenFile(allocs+".metrics", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			f.WriteString(line + "\n")
			f.Close()
		}
	}
	go func() { // (the file is written off the engine and host threads, twice a second)
		for {
			time.Sleep(500 * time.Millisecond)
			l.flush()
			if allocs != "" && allocsFrom > 0 && atomic.LoadUint64(&ovTicks) >= allocsFrom {
				if f, err := os.Create(allocs + ".from"); err == nil {
					pprof.Lookup("allocs").WriteTo(f, 0)
					f.Close()
				}
				allocMetrics("from")
				allocsFrom = 0
			}
			if allocs != "" && atomic.LoadUint64(&ovTicks) >= allocsAt {
				allocMetrics("at")
				if f, err := os.Create(allocs); err == nil {
					pprof.Lookup("allocs").WriteTo(f, 0)
					f.Close()
				}
				allocs = ""
			}
		}
	}()
	return l
}()

var ovTickMarks [10]int64                        // this tick's marks (Ikemen thread only)
var ovTickBetween int64                          // this tick's ovBetweenTicks end (Ikemen thread only)
var ovTickEnter, ovTickRecv, ovTickDrained int64 // the host thread's join entered / tick received, render's GL tasks drained
var ovTickSpeakerUs, ovTickSpeakerN int64        // the Ikemen side's waits for the speaker lock since the last tick line

// ovTickMark: (dev, IKEMEN_TICK_LOG) this tick's phase i reached now
func ovTickMark(i int) {
	if ovTickLog != nil {
		ovTickMarks[i] = ovTimingUs()
	}
}

func (l *ovTickLogT) line(kind byte, vals ...int64) {
	l.mu.Lock()
	l.buf = append(l.buf, kind)
	for _, v := range vals {
		l.buf = append(l.buf, '\t')
		l.buf = strconv.AppendInt(l.buf, v, 10)
	}
	l.buf = append(l.buf, '\n')
	l.mu.Unlock()
}

func (l *ovTickLogT) flush() {
	l.mu.Lock()
	b := l.buf
	l.buf, l.busy = l.busy[:0], b
	l.mu.Unlock()
	if len(b) > 0 {
		l.f.Write(b)
	}
}

// tick: (Ikemen thread) one tick done - from its wake to its done, the publication starting at publish
func (l *ovTickLogT) tick(wake, between, publish, done int64) {
	metrics.Read(l.gc)
	var gc int64
	if l.gc[0].Value.Kind() == metrics.KindUint64 {
		gc = int64(l.gc[0].Value.Uint64())
	}
	m := &ovTickMarks
	l.line('T', int64(ov.tick), wake, between, m[0], m[1], m[2], m[3], m[4], m[5], m[6], m[7], m[8], m[9], publish, done, gc,
		atomic.SwapInt64(&ovTickSpeakerUs, 0), atomic.SwapInt64(&ovTickSpeakerN, 0))
	*m = [10]int64{}
}
