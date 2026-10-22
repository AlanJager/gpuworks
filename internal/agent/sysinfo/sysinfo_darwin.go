//go:build darwin

package sysinfo

import (
	"log/slog"
	"os"
	"runtime"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Collect gathers the host summary on macOS from sysctl and statfs, which the
// Go runtime reaches through libSystem without cgo. Best-effort like the Linux
// collector: a missing source leaves its fields zero.
func Collect() proto.SysInfo {
	s := proto.SysInfo{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUCores: runtime.NumCPU(),
	}
	if h, err := os.Hostname(); err == nil {
		s.Hostname = h
	}
	s.Kernel, _ = unix.Sysctl("kern.osrelease")
	s.CPUModel, _ = unix.Sysctl("machdep.cpu.brand_string")
	if n, err := unix.SysctlUint32("hw.ncpu"); err == nil && n > 0 {
		s.CPUCores = int(n)
	}
	memBytes, _ := unix.SysctlUint64("hw.memsize")
	level, levelErr := unix.SysctlUint32("kern.memorystatus_level")
	s.MemTotalKB, s.MemAvailKB = darwinMem(memBytes, level, levelErr == nil)
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err == nil {
		s.DiskTotalKB, s.DiskAvailKB = statfsKB(uint64(st.Bsize), st.Blocks, st.Bavail)
	}
	if s.MemTotalKB == 0 {
		slog.Warn("host sysinfo incomplete", "cpu_cores", s.CPUCores, "mem_total_kb", s.MemTotalKB)
	}
	return s
}
