package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Called under ov.mu. Prepared roots use the ordinary asynchronous loader but
// remain disabled and unpublished until a real spawn supplies its team/stats/spot.
func ovPrepareSpawn(m map[string]interface{}) (int, error) {
	if ovCfg.ParityMode {
		return -1, nil
	}
	def, _ := m["def"].(string)
	if def == "" {
		return -1, fmt.Errorf("prepare: no def")
	}
	if _, err := os.Stat(ovPath(def)); err != nil {
		return -1, err
	}
	free, count, pn := 0, 0, -1
	for i := 0; i < ov.slotCount; i++ {
		s := &ov.slots[i]
		if s.preparing && !s.cancel {
			if !s.prepareClaimed && !s.replacing && ovPreparedDef(s.def, def) {
				return i, nil
			}
			count++
		}
		if !s.used && len(sys.chars[i]) == 0 {
			free++
			if pn < 0 {
				pn = i
			}
		}
	}
	// Leave room for unplanned live spawns; never evict a fighter to warm a future one.
	if count >= 8 || free <= 8 || pn < 0 {
		return -1, nil
	}
	ovReserveSpawn(pn, map[string]interface{}{"def": def})
	ov.slots[pn].preparing = true
	ov.queue = append(ov.queue, map[string]interface{}{"op": "prepare", "h": float64(pn)})
	return pn, nil
}

func ovClaimPrepared(m map[string]interface{}) int {
	def, _ := m["def"].(string)
	for pn := 0; pn < ov.slotCount; pn++ {
		s := &ov.slots[pn]
		if !s.preparing || s.prepareClaimed || s.replacing || s.cancel || s.loadErr != nil || !ovPreparedDef(s.def, def) {
			continue
		}
		s.prepareClaimed = true
		m["op"], m["h"] = "activate_prepared", float64(pn)
		ov.queue = append(ov.queue, m)
		return pn
	}
	return -1
}

func ovPreparedStatus(m map[string]interface{}) int {
	h, ok := m["h"].(float64)
	if !ok || int(h) < 0 || int(h) >= ov.slotCount {
		return -1
	}
	s := &ov.slots[int(h)]
	def, _ := m["def"].(string)
	if !s.preparing || s.prepareClaimed || s.replacing || s.cancel || !ovPreparedDef(s.def, def) || s.loadErr != nil {
		return -1
	}
	if s.loaded {
		return 1
	}
	return 0
}

func ovClearPrepared() int {
	count := 0
	for pn := 0; pn < ov.slotCount; pn++ {
		if ov.slots[pn].preparing && !ov.slots[pn].prepareClaimed && !ov.slots[pn].replacing {
			ov.queue = append(ov.queue, map[string]interface{}{"op": "despawn", "h": float64(pn)})
			count++
		}
	}
	return count
}

// Loaded, unpublished preparations are safe to replace when the live fight fills
// its slots. A still-running compiler must finish before its slot can be reused.
func ovDiscardablePrepared() int {
	for pn := 0; pn < ov.slotCount; pn++ {
		s := &ov.slots[pn]
		if s.preparing && !s.prepareClaimed && !s.replacing && s.loaded && !s.cancel {
			return pn
		}
	}
	return -1
}

// Apply on the simulation thread, not in the host's command call. A load can finish
// concurrently, so transfer all loader-owned results together under its mutex.
func ovActivatePrepared(pn int, m map[string]interface{}) {
	ov.mu.Lock()
	defer ov.mu.Unlock()
	s := &ov.slots[pn]
	old := *s
	ovReserveSpawn(pn, m)
	s.char, s.loaded, s.loadErr = old.char, old.loaded, old.loadErr
	s.pack, s.ffx, s.ffxName = old.pack, old.ffx, old.ffxName
}

// These definitions come from the same host metadata. Normalize spelling without
// filesystem traversal in the match's warm-status polling path.
func ovPreparedDef(a, b string) bool {
	return a == b || strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
