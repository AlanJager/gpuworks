package gpu

import (
	"context"
	"strconv"
	"strings"
)

// ComputeProcesses lists the processes npu-smi shows on each chip. They come
// from the same `npu-smi info` the probe already read (AscendProber.info
// caches it), so knowing who uses a chip costs no further call into a driver
// that may be slow.
func (p *AscendProber) ComputeProcesses(ctx context.Context) ([]ComputeProcess, error) {
	out, err := p.info(ctx)
	if err != nil {
		return nil, err
	}
	return parseAscendProcesses(out), nil
}

// parseAscendProcesses reads the process table that follows the card table
// of `npu-smi info`:
//
//	| NPU     Chip   | Process id | Process name | Process memory(MB) | Process id in container |
//	| 2       0      | 41234      | python3      | 30215              | 87                      |
//	| No running processes found in NPU 1                                                    |
//
// A row names its chip the way the card table does (NPU id, chip id), so the
// chip's Bus-Id is taken from there. The process id is the host's, which is
// the one a container's engine is known by too; the last column, present on
// newer releases, is the id inside the container and is not used.
func parseAscendProcesses(out string) []ComputeProcess {
	bdf := map[[2]int]string{}
	for _, c := range parseAscendChips(out) {
		bdf[[2]int{c.npu, c.chip}] = c.gpu.PCIeBDF
	}
	var procs []ComputeProcess
	inTable := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Process id") {
			inTable = true
			continue
		}
		if !inTable || !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 3 {
			continue
		}
		ids := strings.Fields(cells[0])
		if len(ids) < 2 {
			continue
		}
		npu, errNPU := strconv.Atoi(ids[0])
		chip, errChip := strconv.Atoi(ids[1])
		pid, errPID := strconv.Atoi(strings.TrimSpace(cells[1]))
		if errNPU != nil || errChip != nil || errPID != nil {
			continue
		}
		address, ok := bdf[[2]int{npu, chip}]
		if !ok {
			// A chip the card table does not list cannot be matched to a
			// card, and guessing one would mark the wrong card busy.
			continue
		}
		proc := ComputeProcess{PID: pid, Name: strings.TrimSpace(cells[2]), PCIeBDF: address}
		if len(cells) > 3 {
			if mb, err := strconv.ParseInt(strings.TrimSpace(cells[3]), 10, 64); err == nil && mb > 0 {
				proc.MemoryBytes = mb << 20
			}
		}
		procs = append(procs, proc)
	}
	return procs
}
