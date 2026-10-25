package gpu

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rootless containers retain the host's DSMI inventory. Physical device IDs
// can be sparse or remapped (for example by VM passthrough), so ACL must use
// the logical IDs reported by the driver, not a compact container ordinal.
func (p *AscendProber) rootlessVisibleDevices(indexes []int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := p.info(ctx)
	if err != nil {
		return "", fmt.Errorf("read Ascend physical devices: %w", err)
	}
	physical := map[int][2]int{}
	for _, chip := range parseAscendChips(info) {
		physical[chip.gpu.Index] = [2]int{chip.npu, chip.chip}
	}
	out, err := p.command(ctx, "info", "-m")
	if err != nil {
		return "", fmt.Errorf("read Ascend logical device mapping: %w", err)
	}
	mapping, err := parseAscendLogicalMapping(out)
	if err != nil {
		return "", err
	}
	visible := make([]string, 0, len(indexes))
	seen := map[int]bool{}
	for _, index := range indexes {
		address, ok := physical[index]
		if !ok {
			return "", fmt.Errorf("Ascend physical device %d is absent from the driver inventory", index)
		}
		logical, ok := mapping[address]
		if !ok {
			return "", fmt.Errorf("Ascend physical device %d has no logical device mapping", index)
		}
		if seen[logical] {
			return "", fmt.Errorf("Ascend logical device %d maps to multiple selected devices", logical)
		}
		seen[logical] = true
		visible = append(visible, strconv.Itoa(logical))
	}
	return strings.Join(visible, ","), nil
}

func parseAscendLogicalMapping(out string) (map[[2]int]int, error) {
	if !strings.Contains(out, "NPU ID") || !strings.Contains(out, "Chip Logic ID") {
		return nil, fmt.Errorf("Ascend logical device mapping has no recognized header")
	}
	result := map[[2]int]int{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] == "-" {
			continue
		}
		npu, e1 := strconv.Atoi(fields[0])
		chip, e2 := strconv.Atoi(fields[1])
		logical, e3 := strconv.Atoi(fields[2])
		if e1 != nil || e2 != nil || e3 != nil || npu < 0 || chip < 0 || logical < 0 {
			continue
		}
		key := [2]int{npu, chip}
		if previous, ok := result[key]; ok && previous != logical {
			return nil, fmt.Errorf("Ascend NPU %d chip %d has conflicting logical device mappings", npu, chip)
		}
		result[key] = logical
	}
	return result, nil
}
