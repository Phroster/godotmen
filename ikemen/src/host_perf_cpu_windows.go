package main

import "syscall"

// hostProcessCPU returns the process's user+kernel CPU seconds (benchmark
// summary only: cpu_ms_per_tick is less sensitive to other load than wall time).
func hostProcessCPU() float64 {
	var c, e, k, u syscall.Filetime
	h, err := syscall.GetCurrentProcess()
	if err != nil || syscall.GetProcessTimes(h, &c, &e, &k, &u) != nil {
		return 0
	}
	ft := func(f syscall.Filetime) float64 {
		return float64(uint64(f.HighDateTime)<<32|uint64(f.LowDateTime)) / 1e7
	}
	return ft(k) + ft(u)
}
