package main

// Packs ship many sound banks containing byte-identical WAVs
// and decoded sprite planes. Only immutable bytes are shared. Sound identity,
// stream positions, sprite palettes, animations and GL textures stay independent.
// Weak indexes never retain a bank after its last actual user goes away.
import (
	"bytes"
	"crypto/sha256"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"sync"
	"weak"
)

var ovAssetsEnabled bool

type ovPixelData struct{ bytes []byte }

var ovAssets = struct {
	sync.Mutex
	waves                                      map[[32]byte]weak.Pointer[Sound]
	pixels                                     map[[32]byte]weak.Pointer[ovPixelData]
	waveHits, pixelHits, waveSaved, pixelSaved uint64
}{waves: make(map[[32]byte]weak.Pointer[Sound]), pixels: make(map[[32]byte]weak.Pointer[ovPixelData])}

func ovConfigureMemory(cfg map[string]interface{}) {
	ovAssetsEnabled = ovOn || ovFFA
	ovCompileCacheOn = ovOn || ovFFA
	if v, ok := cfg["compile_cache"].(bool); ok {
		ovCompileCacheOn = ovCompileCacheOn && v
	}
	ovCompileCacheVerify, _ = cfg["compile_cache_verify"].(bool)
	if v, ok := cfg["asset_sharing"].(bool); ok {
		ovAssetsEnabled = ovAssetsEnabled && v
	}
	if !ovOn && !ovFFA {
		return
	}
	// The GC paces at 200 % with a memory limit: a large fight keeps several GB of sprites and sounds live, and the
	// default 100 % would mark that heap over and over. The pace changes no value the game computes. ovConfigure keys
	// gc_percent / memory_limit_mb; env IKEMEN_GC_PERCENT / IKEMEN_MEMORY_LIMIT_MB override them; the replay budget is
	// added to the limit.
	gc, limitMB := 200, int64(8192)
	// 12 GB of limit by default, so the GC is not limit-bound in a 100-fighter fight. Env IKEMEN_GC_HEADROOM=0: 8 GB
	if os.Getenv("IKEMEN_GC_HEADROOM") != "0" {
		limitMB = 12288
	}
	if v, ok := cfg["gc_percent"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 10 && v <= 1000 {
		gc = int(v)
	}
	if v, ok := cfg["memory_limit_mb"].(float64); ok && !math.IsNaN(v) && !math.IsInf(v, 0) && (v == 0 || v >= 256 && v <= 65536) {
		limitMB = int64(v)
	}
	// (dev A/B: env IKEMEN_GC_PERCENT / IKEMEN_MEMORY_LIMIT_MB override the two - the GC's pace only)
	if v, err := strconv.Atoi(os.Getenv("IKEMEN_GC_PERCENT")); err == nil && v >= 10 && v <= 1000 {
		gc = v
	}
	if v, err := strconv.ParseInt(os.Getenv("IKEMEN_MEMORY_LIMIT_MB"), 10, 64); err == nil && (v == 0 || v >= 256 && v <= 65536) {
		limitMB = v
	}
	debug.SetGCPercent(gc)
	limit := int64(math.MaxInt64)
	if limitMB > 0 {
		limit = limitMB << 20
	}
	debug.SetMemoryLimit(limit)
	ovLog("memory: asset_sharing %v, gc_percent %d, memory_limit_mb %d (Go runtime only)", ovAssetsEnabled, gc, limitMB)
}

func ovWaveCopy(sound *Sound) *Sound {
	// Stop(sound) compares the *Sound identity; each SND entry must keep its own.
	return &Sound{wavData: sound.wavData, format: sound.format, length: sound.length, ovShared: sound}
}

func ovFindWave(data []byte) (*Sound, [32]byte) {
	if !ovAssetsEnabled {
		return nil, [32]byte{}
	}
	key := sha256.Sum256(data)
	ovAssets.Lock()
	defer ovAssets.Unlock()
	if sound := ovAssets.waves[key].Value(); sound != nil && bytes.Equal(sound.wavData, data) {
		ovAssets.waveHits++
		ovAssets.waveSaved += uint64(len(data))
		return ovWaveCopy(sound), key
	}
	return nil, key
}

func ovStoreWave(sound *Sound, key [32]byte) *Sound {
	if !ovAssetsEnabled {
		return sound
	}
	ovAssets.Lock()
	defer ovAssets.Unlock()
	// Two loader workers can validate the same first wave concurrently.
	if existing := ovAssets.waves[key].Value(); existing != nil && bytes.Equal(existing.wavData, sound.wavData) {
		ovAssets.waveHits++
		ovAssets.waveSaved += uint64(len(sound.wavData))
		return ovWaveCopy(existing)
	}
	// Index growth is bounded even across many spawn / despawn cycles.
	if len(ovAssets.waves) >= 32768 {
		ovAssets.waves = make(map[[32]byte]weak.Pointer[Sound])
	}
	ovAssets.waves[key] = weak.Make(sound)
	return sound
}

func ovSharePixels(data []byte) ([]byte, *ovPixelData) {
	if !ovAssetsEnabled || len(data) < 1024 {
		return data, nil
	}
	key := sha256.Sum256(data)
	ovAssets.Lock()
	defer ovAssets.Unlock()
	if existing := ovAssets.pixels[key].Value(); existing != nil && bytes.Equal(existing.bytes, data) {
		ovAssets.pixelHits++
		ovAssets.pixelSaved += uint64(len(data))
		return existing.bytes, existing
	}
	if len(ovAssets.pixels) >= 65536 {
		ovAssets.pixels = make(map[[32]byte]weak.Pointer[ovPixelData])
	}
	shared := &ovPixelData{bytes: data}
	ovAssets.pixels[key] = weak.Make(shared)
	return data, shared
}

func ovAssetCounts() map[string]uint64 {
	ovAssets.Lock()
	defer ovAssets.Unlock()
	return map[string]uint64{"wave_hits": ovAssets.waveHits, "pixel_hits": ovAssets.pixelHits,
		"wave_bytes_avoided": ovAssets.waveSaved, "pixel_bytes_avoided": ovAssets.pixelSaved,
		"wave_index_entries": uint64(len(ovAssets.waves)), "pixel_index_entries": uint64(len(ovAssets.pixels))}
}
