//go:build linux

package sysinfo

import (
	"strings"
	"testing"
)

func TestLinuxProcCPUAndMemFixtures(t *testing.T) {
	model, cores := parseCPU(strings.NewReader("processor : 0\nmodel name : Fixture CPU\nprocessor : 1\n"))
	if model != "Fixture CPU" || cores != 2 {
		t.Fatalf("parseCPU = %q/%d", model, cores)
	}
	total, available := parseMem(strings.NewReader("MemTotal:       123456 kB\nMemAvailable:    65432 kB\nMalformed\n"))
	if total != 123456 || available != 65432 {
		t.Fatalf("parseMem = %d/%d", total, available)
	}
	if got := charsToString([]int8{'a', 'b', 0, 'c'}); got != "ab" {
		t.Fatalf("charsToString = %q", got)
	}
}

func TestLinuxCollectReadsHostFactsWithoutPanic(t *testing.T) {
	got := Collect()
	if got.OS != "linux" || got.Arch == "" || got.Hostname == "" {
		t.Fatalf("Collect = %+v", got)
	}
	if got.CPUCores <= 0 || got.MemTotalKB <= 0 || got.DiskTotalKB <= 0 {
		t.Fatalf("Collect resource facts = %+v", got)
	}
}
