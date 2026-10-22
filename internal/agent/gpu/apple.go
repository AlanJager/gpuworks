package gpu

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

type appleCommandRunner func(ctx context.Context, name string, args ...string) (string, error)

// AppleProber detects the GPU of an Apple silicon Mac. The SoC has exactly
// one GPU and it shares the host's memory, so the node reports one card whose
// memory is the part of host memory Metal lets the GPU keep resident. Like the
// other probers it shells out (sysctl, ioreg) instead of linking Metal, so the
// agent stays a CGO-free static binary; neither command needs root.
type AppleProber struct {
	run  appleCommandRunner
	goos string

	uuidMu sync.Mutex
	uuidOf string // the platform UUID once read; it never changes
}

func (p *AppleProber) Vendor() string { return accelerator.VendorApple }

// Available reports an Apple silicon Mac. An Intel Mac answers sysctl too,
// with an Intel brand string, and has no Metal GPU the engines can use.
func (p *AppleProber) Available() bool {
	if p.os() != "darwin" {
		return false
	}
	facts, _ := p.sysctl(context.Background())
	return facts.appleSilicon()
}

func (p *AppleProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	if p.os() != "darwin" {
		return nil, nil
	}
	facts, err := p.sysctl(ctx)
	if err != nil {
		return nil, err
	}
	if !facts.appleSilicon() {
		return nil, nil
	}
	accel, err := p.accelerator(ctx)
	if err != nil {
		return nil, err
	}
	name := accel.model
	if name == "" {
		name = facts.brand
	}
	return []proto.GPUInfo{{
		Index:            0,
		UUID:             p.uuid(ctx),
		Name:             name,
		MemoryTotalBytes: appleGPUWorkingSet(facts.memBytes, facts.wiredLimitMB),
		Vendor:           accelerator.VendorApple,
		ArchID:           appleChip(name),
		NUMANode:         -1,
		DriverVersion:    facts.osVersion,
		Healthy:          true,
	}}, nil
}

// DriverVersion is the macOS version: Metal and the GPU driver ship with the
// operating system and have no version of their own to report.
func (p *AppleProber) DriverVersion(ctx context.Context) string {
	facts, _ := p.sysctl(ctx)
	return facts.osVersion
}

func (p *AppleProber) Framework(ctx context.Context) (string, string) {
	return "metal", p.DriverVersion(ctx)
}

// Topology is empty: one GPU has no peers to describe.
func (p *AppleProber) Topology(context.Context) string { return "" }

func (p *AppleProber) Metrics(ctx context.Context) ([]Metric, error) {
	facts, err := p.sysctl(ctx)
	if err != nil {
		return nil, err
	}
	accel, err := p.accelerator(ctx)
	if err != nil {
		return nil, err
	}
	values := map[string]float64{
		"gpuworks_gpu_memory_total_bytes": float64(appleGPUWorkingSet(facts.memBytes, facts.wiredLimitMB)),
	}
	if accel.utilization >= 0 {
		values["gpuworks_gpu_utilization"] = accel.utilization
	}
	if accel.inUseBytes >= 0 {
		values["gpuworks_gpu_memory_used_bytes"] = float64(accel.inUseBytes)
	}
	return []Metric{{Index: 0, UUID: p.uuid(ctx), Vendor: accelerator.VendorApple, Values: values}}, nil
}

// RuntimeInjection has nothing to add: engines run as host processes that see
// the one GPU through Metal, so selecting it needs no device, mount or
// variable. Any index but 0 names a GPU the Mac does not have.
func (p *AppleProber) RuntimeInjection(indexes []int) RuntimeInjection {
	for _, index := range indexes {
		if index != 0 {
			return RuntimeInjection{UnsupportedReason: fmt.Sprintf("an Apple silicon Mac has one GPU, index 0; index %d does not exist", index)}
		}
	}
	return RuntimeInjection{}
}

func (p *AppleProber) os() string {
	if p.goos != "" {
		return p.goos
	}
	return runtime.GOOS
}

func (p *AppleProber) exec(ctx context.Context, name string, args ...string) (string, error) {
	if p.run != nil {
		return p.run(ctx, name, args...)
	}
	return RunVendorCLI(ctx, nil, name, args...)
}

// appleSysctl holds the sysctl facts the probe reads in one call.
type appleSysctl struct {
	memBytes     int64
	wiredLimitMB int64
	osVersion    string
	brand        string
}

func (f appleSysctl) appleSilicon() bool {
	return strings.HasPrefix(f.brand, "Apple ") && f.memBytes > 0
}

// sysctl asks for every key in one call, with names, because sysctl prints the
// keys it knows and exits non-zero for any it does not (iogpu.* does not exist
// before Apple silicon): the output still carries everything that exists.
func (p *AppleProber) sysctl(ctx context.Context) (appleSysctl, error) {
	out, err := p.exec(ctx, "sysctl", "hw.memsize", "iogpu.wired_limit_mb", "kern.osproductversion", "machdep.cpu.brand_string")
	facts := parseAppleSysctl(out)
	if facts.memBytes == 0 && err != nil {
		return facts, fmt.Errorf("sysctl: %w", err)
	}
	return facts, nil
}

func parseAppleSysctl(out string) appleSysctl {
	var facts appleSysctl
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "hw.memsize":
			facts.memBytes, _ = strconv.ParseInt(value, 10, 64)
		case "iogpu.wired_limit_mb":
			facts.wiredLimitMB, _ = strconv.ParseInt(value, 10, 64)
		case "kern.osproductversion":
			facts.osVersion = value
		case "machdep.cpu.brand_string":
			facts.brand = value
		}
	}
	return facts
}

// appleAccelerator holds what the GPU's IOAccelerator registry entry says.
// A negative number is a value the entry did not carry.
type appleAccelerator struct {
	model       string
	utilization float64
	inUseBytes  int64
}

func (p *AppleProber) accelerator(ctx context.Context) (appleAccelerator, error) {
	out, err := p.exec(ctx, "ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	if err != nil {
		return appleAccelerator{utilization: -1, inUseBytes: -1}, fmt.Errorf("ioreg: %w", err)
	}
	return parseAppleAccelerator(out), nil
}

func parseAppleAccelerator(out string) appleAccelerator {
	accel := appleAccelerator{utilization: -1, inUseBytes: -1}
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := ioregProperty(line)
		if !ok {
			continue
		}
		switch key {
		case "model":
			accel.model = strings.Trim(value, `"`)
		case "PerformanceStatistics":
			stats := ioregDictionary(value)
			if v, ok := stats["Device Utilization %"]; ok {
				if n, err := strconv.ParseFloat(v, 64); err == nil {
					accel.utilization = n
				}
			}
			if v, ok := stats["In use system memory"]; ok {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					accel.inUseBytes = n
				}
			}
		}
	}
	return accel
}

// uuid is the Mac's platform UUID: the GPU is soldered to the SoC, so the
// machine's identity is the GPU's, and it survives reboots and OS updates.
// It is read once: every metrics sample carries it.
func (p *AppleProber) uuid(ctx context.Context) string {
	p.uuidMu.Lock()
	defer p.uuidMu.Unlock()
	if p.uuidOf == "" {
		p.uuidOf = p.readUUID(ctx)
	}
	return p.uuidOf
}

func (p *AppleProber) readUUID(ctx context.Context) string {
	out, err := p.exec(ctx, "ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := ioregProperty(line); ok && key == "IOPlatformUUID" {
			if id := strings.Trim(value, `"`); id != "" {
				return "APPLE-" + id
			}
		}
	}
	return ""
}

// ioregProperty splits one `"key" = value` line of ioreg's output.
func ioregProperty(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, `"`) {
		return "", "", false
	}
	end := strings.Index(line[1:], `"`)
	if end < 0 {
		return "", "", false
	}
	key := line[1 : end+1]
	rest := strings.TrimSpace(line[end+2:])
	value, ok := strings.CutPrefix(rest, "=")
	if !ok {
		return "", "", false
	}
	return key, strings.TrimSpace(value), true
}

// ioregDictionary reads the flat `{"key"=value,...}` form ioreg prints for a
// dictionary property. Nested values are not needed and are skipped whole.
func ioregDictionary(value string) map[string]string {
	result := map[string]string{}
	body := strings.TrimSpace(value)
	body = strings.TrimPrefix(body, "{")
	body = strings.TrimSuffix(body, "}")
	depth := 0
	start := 0
	flush := func(end int) {
		entry := body[start:end]
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			return
		}
		result[strings.Trim(strings.TrimSpace(key), `"`)] = strings.TrimSpace(val)
	}
	for i, r := range body {
		switch r {
		case '{', '(':
			depth++
		case '}', ')':
			depth--
		case ',':
			if depth == 0 {
				flush(i)
				start = i + 1
			}
		}
	}
	if start < len(body) {
		flush(len(body))
	}
	return result
}

// appleChip is the SoC name ("M1 Max") of a GPU name ("Apple M1 Max").
func appleChip(name string) string {
	return accelerator.NormalizeGPUName(name)
}

// appleGPUWorkingSet is the part of host memory Metal lets the GPU keep
// resident, which is the memory an engine can use: vllm-metal sizes
// --gpu-memory-utilization against it (Metal's recommendedMaxWorkingSetSize).
// Reading it directly needs the Metal API and therefore cgo, so it is derived
// the way macOS derives it: the iogpu.wired_limit_mb an administrator set,
// otherwise two thirds of memory up to 32 GiB and three quarters above. The
// rule matches the value Metal reports on every size checked (8, 16, 24, 32,
// 36, 64, 128 and 192 GiB, as logged by llama.cpp; 64 GiB measured on an M1
// Max).
func appleGPUWorkingSet(memBytes, wiredLimitMB int64) int64 {
	if memBytes <= 0 {
		return 0
	}
	if wiredLimitMB > 0 {
		return min(wiredLimitMB<<20, memBytes)
	}
	if memBytes > 32<<30 {
		return memBytes * 3 / 4
	}
	return memBytes * 2 / 3
}
