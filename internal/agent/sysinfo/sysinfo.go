//go:build linux

// Package sysinfo collects host facts from /proc, sysfs and syscalls.
// No cgo: everything is file parsing or golang.org/x/sys-free syscalls.
package sysinfo

import (
	"bufio"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Collect gathers a host summary. Best-effort: missing sources leave zero values.
func Collect() proto.SysInfo {
	slog.Debug("collecting host sysinfo")
	s := proto.SysInfo{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}
	if h, err := os.Hostname(); err == nil {
		s.Hostname = h
	}
	s.Kernel = readKernel()
	s.CPUModel, s.CPUCores = readCPU()
	s.MemTotalKB, s.MemAvailKB = readMem()
	s.DiskTotalKB, s.DiskAvailKB = readDisk("/")
	if s.CPUCores == 0 || s.MemTotalKB == 0 {
		slog.Warn("host sysinfo incomplete", "cpu_cores", s.CPUCores, "mem_total_kb", s.MemTotalKB)
	}
	return s
}

func readKernel() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	return charsToString(u.Release[:])
}

func charsToString(ca []int8) string {
	b := make([]byte, 0, len(ca))
	for _, c := range ca {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}

func readCPU() (model string, cores int) {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return "", runtime.NumCPU()
	}
	defer f.Close()
	return parseCPU(f)
}

func parseCPU(r io.Reader) (model string, cores int) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if model == "" && strings.HasPrefix(line, "model name") {
			if i := strings.IndexByte(line, ':'); i >= 0 {
				model = strings.TrimSpace(line[i+1:])
			}
		}
		if strings.HasPrefix(line, "processor") {
			cores++
		}
	}
	if cores == 0 {
		cores = runtime.NumCPU()
	}
	return model, cores
}

func readMem() (totalKB, availKB int64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	return parseMem(f)
}

func parseMem(r io.Reader) (totalKB, availKB int64) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		}
	}
	return totalKB, availKB
}

func readDisk(path string) (totalKB, availKB int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	totalKB = int64(st.Blocks) * bs / 1024
	availKB = int64(st.Bavail) * bs / 1024
	return totalKB, availKB
}
