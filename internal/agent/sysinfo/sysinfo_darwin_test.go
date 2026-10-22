//go:build darwin

package sysinfo

import "testing"

func TestDarwinCollectReadsTheHost(t *testing.T) {
	got := Collect()
	if got.OS != "darwin" || got.Arch == "" || got.CPUCores <= 0 {
		t.Fatalf("darwin Collect = %+v", got)
	}
	if got.MemTotalKB <= 0 || got.MemAvailKB <= 0 || got.MemAvailKB > got.MemTotalKB {
		t.Fatalf("memory = total %d kB, available %d kB", got.MemTotalKB, got.MemAvailKB)
	}
	if got.DiskTotalKB <= 0 || got.DiskAvailKB <= 0 || got.Kernel == "" || got.CPUModel == "" {
		t.Fatalf("disk/kernel/cpu = %+v", got)
	}
}
