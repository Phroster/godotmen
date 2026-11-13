package main

// Overlay loader diagnostics. No server, extra process, or gameplay changes.
// IKEMEN_MEMORY_PROFILE=<absolute output directory> writes heap/alloc profiles once
// IKEMEN_MEMORY_FIGHTERS ready roots have settled for 120 ticks without loaders.
import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"time"
)

var ovMemoryProfile = struct {
	dir            string
	count, settled int
	done           bool
}{dir: os.Getenv("IKEMEN_MEMORY_PROFILE")}

func ovMemoryTick() {
	if ovMemoryProfile.dir == "" || ovMemoryProfile.done {
		return
	}
	if ovMemoryProfile.count == 0 {
		ovMemoryProfile.count, _ = strconv.Atoi(os.Getenv("IKEMEN_MEMORY_FIGHTERS"))
		if ovMemoryProfile.count < 2 {
			ovMemoryProfile.count = 32
		}
	}
	n, loading := 0, 0
	ov.mu.Lock()
	for i := range ov.slots {
		if ov.slots[i].ready {
			n++
		}
		if ov.slots[i].loading {
			loading++
		}
	}
	ov.mu.Unlock()
	if n != ovMemoryProfile.count || loading != 0 {
		ovMemoryProfile.settled = 0
		return
	}
	ovMemoryProfile.settled++
	if ovMemoryProfile.settled != 120 {
		return
	}
	ovMemoryProfile.done = true
	if err := ovWriteMemoryProfiles(ovMemoryProfile.dir, n); err != nil {
		ovLog("memory profile: %v", err)
	}
}

func ovWriteMemoryProfiles(dir string, fighters int) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("memory profile directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	writeProfile := func(name, kind string) error {
		f, err := os.Create(filepath.Join(dir, name+".pprof"))
		if err != nil {
			return err
		}
		err = pprof.Lookup(kind).WriteTo(f, 0)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := writeProfile("heap-before-gc", "heap"); err != nil {
		return err
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if err := writeProfile("heap", "heap"); err != nil {
		return err
	}
	if err := writeProfile("allocs", "allocs"); err != nil {
		return err
	}
	type bank struct {
		File                                                       string
		Sprites, Decoded, Compressed, Textures                     int
		PixelBytes, SoundBytes, UniquePixelBytes, UniqueSoundBytes int64
	}
	sffs, snds := map[*Sff]bool{}, map[*Snd]bool{}
	pixels, waves := map[*byte]bool{}, map[*byte]bool{}
	var banks []bank
	addSff := func(s *Sff) {
		if s == nil || sffs[s] {
			return
		}
		sffs[s] = true
		b := bank{File: s.filename, Sprites: len(s.sprites)}
		for _, spr := range s.sprites {
			if spr == nil {
				continue
			}
			if len(spr.ovPx) > 0 {
				b.Decoded++
				b.PixelBytes += int64(len(spr.ovPx))
				key := &spr.ovPx[0]
				if !pixels[key] {
					b.UniquePixelBytes += int64(len(spr.ovPx))
					pixels[key] = true
				}
			}
			if spr.ovDecode != nil {
				b.Compressed++
			}
			if spr.Tex != nil {
				b.Textures++
			}
		}
		banks = append(banks, b)
	}
	addSnd := func(s *Snd) {
		if s == nil || snds[s] {
			return
		}
		snds[s] = true
		b := bank{File: s.filename}
		for _, sound := range s.table {
			if sound != nil && len(sound.wavData) > 0 {
				b.SoundBytes += int64(len(sound.wavData))
				key := &sound.wavData[0]
				if !waves[key] {
					b.UniqueSoundBytes += int64(len(sound.wavData))
					waves[key] = true
				}
			}
		}
		banks = append(banks, b)
	}
	states, commands := 0, 0
	for pn := range sys.cgi {
		gi := &sys.cgi[pn]
		addSff(gi.sff)
		addSnd(gi.snd)
		states += len(gi.states)
		if len(sys.chars[pn]) > 0 {
			for _, cl := range sys.chars[pn][0].cmd {
				commands += len(cl.Commands)
			}
		}
	}
	for _, fx := range sys.ffx {
		if fx != nil {
			addSff(fx.sff)
			addSnd(fx.snd)
		}
	}
	rec := map[string]interface{}{"fighters": fighters, "time": time.Now().Format(time.RFC3339Nano),
		"before_gc": before, "after_gc": after, "banks": banks, "state_count": states, "command_groups": commands, "asset_sharing": ovAssetCounts(), "compile_cache": ovExpressionCounts()}
	js, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(dir, "memory.json"), append(js, '\n'), 0644); err != nil {
		return err
	}
	ovEvent(`{"e":"memory_profile","fighters":%d,"heap_alloc":%d,"heap_inuse":%d,"heap_sys":%d,"before_heap_alloc":%d}`, fighters, after.HeapAlloc, after.HeapInuse, after.HeapSys, before.HeapAlloc)
	ovLog("memory profiled %d fighters: live %.1f MiB, inuse %.1f MiB, reserved %.1f MiB", fighters, float64(after.HeapAlloc)/(1<<20), float64(after.HeapInuse)/(1<<20), float64(after.HeapSys)/(1<<20))
	return nil
}
