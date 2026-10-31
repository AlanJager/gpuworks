package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Who uses each Hygon card, read from the KFD driver's own sysfs.
//
// This is where `hy-smi --showpids` gets its answer too (it is a rocm-smi
// fork), so reading it directly costs no process start and cannot hang on a
// stuck tool:
//
//	/sys/class/kfd/kfd/topology/nodes/<n>/gpu_id      the card's KFD id (0 is a CPU)
//	/sys/class/kfd/kfd/topology/nodes/<n>/properties  "domain" and "location_id" (bus<<8 | dev<<3 | fn)
//	/sys/class/kfd/kfd/proc/<pid>/queues/<q>/gpuid    a queue the process created on that card
//	/sys/class/kfd/kfd/proc/<pid>/vram_<gpu_id>       bytes of the card's memory it holds
//
// The pid is the host's, so a container's engine is known by it too.
//
// A process is on a card when it created a queue there. Memory alone is not
// the test: the runtime opens every card it can see, and a process whose
// HIP_VISIBLE_DEVICES names one card would otherwise read as using all of
// them, taking idle cards away from scheduling. Only a kernel that exposes no
// queues at all falls back to memory held.

const kfdRoot = "sys/class/kfd/kfd"

// ComputeProcesses lists the KFD processes on each Hygon card.
func (p *HygonProber) ComputeProcesses(ctx context.Context) ([]ComputeProcess, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root := p.hostRoot
	if root == "" {
		root = "/"
	}
	cards, err := p.kfdCards(root)
	if err != nil {
		return nil, err
	}
	dirs, err := p.match(filepath.Join(root, kfdRoot, "proc", "*"))
	if err != nil {
		return nil, fmt.Errorf("cannot list KFD processes: %w", err)
	}
	if len(dirs) == 0 {
		// An empty proc directory is "nobody", but a missing one is a
		// driver this reader does not understand: not knowing must not
		// read as every card being free.
		if _, err := p.inspect(filepath.Join(root, kfdRoot, "proc")); err != nil {
			return nil, fmt.Errorf("KFD process directory unavailable: %w", err)
		}
	}
	var procs []ComputeProcess
	for _, dir := range dirs {
		pid, err := strconv.Atoi(filepath.Base(dir))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(p.readQuiet(filepath.Join(root, "proc", strconv.Itoa(pid), "comm"))))
		on := p.kfdQueueCards(dir)
		for _, gpuID := range sortedKeys(cards) {
			vram := parseKFDInt(p.readQuiet(filepath.Join(dir, "vram_"+gpuID)))
			if on != nil && !on[gpuID] {
				continue
			}
			if on == nil && vram <= 0 {
				continue
			}
			procs = append(procs, ComputeProcess{PID: pid, Name: name, PCIeBDF: cards[gpuID], MemoryBytes: vram})
		}
	}
	return procs, nil
}

// kfdCards maps each card's KFD gpu_id to its PCI address.
func (p *HygonProber) kfdCards(root string) (map[string]string, error) {
	nodes, err := p.match(filepath.Join(root, kfdRoot, "topology", "nodes", "*"))
	if err != nil {
		return nil, fmt.Errorf("cannot list KFD topology: %w", err)
	}
	cards := map[string]string{}
	for _, node := range nodes {
		gpuID := strings.TrimSpace(string(p.readQuiet(filepath.Join(node, "gpu_id"))))
		if gpuID == "" || gpuID == "0" {
			continue
		}
		props, err := p.read(filepath.Join(node, "properties"))
		if err != nil {
			continue
		}
		if bdf := kfdNodeBDF(string(props)); bdf != "" {
			cards[gpuID] = bdf
		}
	}
	if len(cards) == 0 {
		return nil, errors.New("KFD topology lists no cards")
	}
	return cards, nil
}

// kfdQueueCards is the set of gpu_ids a process has queues on, or nil when
// the kernel does not expose its queues.
func (p *HygonProber) kfdQueueCards(dir string) map[string]bool {
	if _, err := p.inspect(filepath.Join(dir, "queues")); err != nil {
		return nil
	}
	on := map[string]bool{}
	queues, _ := p.match(filepath.Join(dir, "queues", "*", "gpuid"))
	for _, q := range queues {
		if id := strings.TrimSpace(string(p.readQuiet(q))); id != "" {
			on[id] = true
		}
	}
	return on
}

// kfdNodeBDF reads a topology node's PCI address from its properties.
func kfdNodeBDF(props string) string {
	domain, location := int64(-1), int64(-1)
	for _, line := range strings.Split(props, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "domain":
			domain = n
		case "location_id":
			location = n
		}
	}
	if domain < 0 || location <= 0 {
		return ""
	}
	return fmt.Sprintf("%04x:%02x:%02x.%x", domain, location>>8, (location>>3)&0x1f, location&0x7)
}

func parseKFDInt(b []byte) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// readQuiet reads a sysfs file that may vanish under it (a process exits
// between the listing and the read); a missing file reads as empty.
func (p *HygonProber) readQuiet(path string) []byte {
	b, err := p.read(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return b
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
