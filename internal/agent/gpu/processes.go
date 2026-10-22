package gpu

import (
	"context"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// ComputeProcess is a compute process the vendor tool lists on a card.
type ComputeProcess struct {
	PID  int
	Name string
	// PCIeBDF is the card's address as the vendor tool prints it.
	PCIeBDF string
	// MemoryBytes is what the process holds on the card, 0 when the tool
	// does not report it.
	MemoryBytes int64
}

// ComputeProcessLister is implemented by probers whose vendor tool lists the
// compute processes on each card. Doctor uses it to name what must stop
// before a card can be reset.
type ComputeProcessLister interface {
	ComputeProcesses(ctx context.Context) ([]ComputeProcess, error)
}

// ComputeProcesses lists compute processes via ppu-smi.
func (p *PPUProber) ComputeProcesses(ctx context.Context) ([]ComputeProcess, error) {
	out, err := runPPU(ctx, "--query-compute-apps=pid,process_name,pci.bus_id", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parseComputeProcesses(out), nil
}

// ComputeProcesses lists compute processes via nvidia-smi. The memory each
// holds comes with them: a card is not offered for work while somebody
// else's process holds memory on it, and how much makes the report worth
// reading.
func (p *SMIProber) ComputeProcesses(ctx context.Context) ([]ComputeProcess, error) {
	out, err := p.query(ctx, "--query-compute-apps=pid,process_name,gpu_bus_id,used_gpu_memory", "--format=csv,noheader,nounits")
	if err == nil {
		return parseComputeProcesses(out), nil
	}
	if IsCLIUnresponsive(err) {
		return nil, err
	}
	// An older driver does not know used_gpu_memory for compute apps.
	out, err = p.query(ctx, "--query-compute-apps=pid,process_name,gpu_bus_id", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, err
	}
	return parseComputeProcesses(out), nil
}

// parseComputeProcesses reads "pid, process name, bus id" rows, optionally
// followed by the memory in MiB. A process name may itself contain a comma,
// so the pid comes first, the address and any memory from the end, and
// everything between is the name.
func parseComputeProcesses(out string) []ComputeProcess {
	var procs []ComputeProcess
	for _, line := range strings.Split(out, "\n") {
		f := splitCSV(line)
		if len(f) < 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		last := len(f) - 1
		var memoryBytes int64
		// The memory column is present only when it was asked for, and
		// reads "[N/A]" on a card that does not account for it.
		if mib, err := strconv.ParseInt(strings.TrimSpace(f[last]), 10, 64); err == nil && last >= 3 {
			memoryBytes = mib << 20
			last--
		} else if last >= 3 && strings.HasPrefix(strings.TrimSpace(f[last]), "[") {
			last--
		}
		procs = append(procs, ComputeProcess{
			PID:         pid,
			Name:        strings.Join(f[1:last], ", "),
			PCIeBDF:     f[last],
			MemoryBytes: memoryBytes,
		})
	}
	return procs
}

// SameBDF reports whether two PCI addresses name the same function, however
// many domain digits each tool prints ("00000000:7E:00.0" and "0000:7e:00.0").
func SameBDF(a, b string) bool {
	return a != "" && accelerator.NormalizeBDF(a) == accelerator.NormalizeBDF(b)
}
