package gpu

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// ascendChip is one chip of the `npu-smi info` card table: the GPU it is
// reported as, and how npu-smi addresses it (`-i <npu> -c <chip>`).
type ascendChip struct {
	gpu  proto.GPUInfo
	npu  int
	chip int
	// first is the chip's first table line; row adds its Bus-Id line when
	// the table spans two lines per chip (npu-smi 23.x and later).
	first string
	row   string
}

// parseAscendChips reads the card table of `npu-smi info`. Every chip is its
// own device, with its own /dev/davinciN, PCIe function, memory and health.
// How a chip is numbered depends on the card's layout, identified the way
// zstack-utility does (zstacklib gpu/vendors/huawei_common.py,
// resolve_handler):
//
//   - One chip per card (910B): one chip row per NPU and no Phy-ID column.
//     The device index is the NPU id.
//   - Several chips per card (910C): the table has a Phy-ID column, an NPU
//     has more than one chip id, or the chip name says 910C. The device index
//     is the chip's Phy-ID, the number /dev/davinciN, ASCEND_VISIBLE_DEVICES,
//     hccn_tool and `npu-smi info -t topo` use (docs/dev/accelerators.md).
//     A chip of such a card without a Phy-ID cannot be addressed and is left
//     out rather than given a wrong device.
//
// A multi-chip card whose chips are named Ascend910 is a 910C; the
// first-generation 910 is the one-chip card of that name.
func parseAscendChips(out string) []ascendChip {
	lines := strings.Split(out, "\n")
	phyColumn := false
	for _, line := range lines {
		if strings.Contains(line, "Phy-ID") {
			phyColumn = true
			break
		}
	}
	type tableChip struct {
		npu, chip, phy int
		name           string
		first, row     string
	}
	var rows []tableChip
	seen := map[[2]int]bool{}
	chipIDs := map[int]map[int]bool{}
	for i, line := range lines {
		fields := strings.Fields(strings.NewReplacer("|", " ", "/", " ").Replace(line))
		if len(fields) < 2 {
			continue
		}
		npu, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		name := ascendName(fields)
		if name == "" {
			continue
		}
		row := ascendTableRow(lines, i)
		chip, phy := ascendChipIDs(row)
		key := [2]int{npu, chip}
		if seen[key] {
			continue
		}
		seen[key] = true
		if chipIDs[npu] == nil {
			chipIDs[npu] = map[int]bool{}
		}
		chipIDs[npu][chip] = true
		rows = append(rows, tableChip{npu: npu, chip: chip, phy: phy, name: name, first: line, row: row})
	}
	chips := make([]ascendChip, 0, len(rows))
	for _, r := range rows {
		model := ascendModel(r.name)
		multiChip := phyColumn || len(chipIDs[r.npu]) > 1 || strings.Contains(strings.ToUpper(model), "910C")
		index := r.npu
		if multiChip {
			if r.phy < 0 {
				slog.Warn("ascend chip has no physical id; left out", "vendor", accelerator.VendorAscend, "npu", r.npu, "chip", r.chip)
				continue
			}
			index = r.phy
			if model == "910" {
				model = "910C"
			}
		}
		fields := strings.Fields(strings.NewReplacer("|", " ", "/", " ").Replace(r.first))
		mem := ascendMemoryBytes(fields)
		if mem == 0 {
			// npu-smi 26.x renders HBM as the final `used / total` pair
			// without a unit in the table. The column is documented in MB.
			if _, total, ok := parseSlashBytesWithDefaultUnit(r.row, 1<<20); ok {
				mem = int64(total)
			}
		}
		health := ascendHealthStatus(fields)
		code := ascendHealthCode(health)
		var evidence []accelerator.HealthField
		if code != "" {
			evidence = ascendEvidence(r.npu, r.chip, health)
		}
		gpu := proto.GPUInfo{
			Index:            index,
			UUID:             "ascend-" + strconv.Itoa(index),
			Name:             "Ascend " + model,
			MemoryTotalBytes: mem,
			Vendor:           accelerator.VendorAscend,
			ArchID:           model,
			PCIeBDF:          ascendBDF(r.row),
			NUMANode:         -1,
		}
		SetCardFinding(&gpu, code, evidence)
		chips = append(chips, ascendChip{gpu: gpu, npu: r.npu, chip: r.chip, first: r.first, row: r.row})
	}
	return chips
}

func parseAscendInfo(out string) []proto.GPUInfo {
	chips := parseAscendChips(out)
	gpus := make([]proto.GPUInfo, len(chips))
	for i, c := range chips {
		gpus[i] = c.gpu
	}
	return gpus
}

// ascendChipIDs reads the chip id and Phy-ID in front of the Bus-Id on a
// chip's second table line ("| 0     2    | 0000:99:00.0 | ..."). A table
// without that line has one chip per card: chip 0, no Phy-ID (-1).
func ascendChipIDs(row string) (chip, phy int) {
	_, second, ok := strings.Cut(row, "\n")
	if !ok {
		return 0, -1
	}
	cells := strings.Split(strings.Trim(strings.TrimSpace(second), "|"), "|")
	ids := strings.Fields(cells[0])
	chip, phy = 0, -1
	if len(ids) > 0 {
		if n, err := strconv.Atoi(ids[0]); err == nil {
			chip = n
		}
	}
	if len(ids) > 1 {
		if n, err := strconv.Atoi(ids[1]); err == nil {
			phy = n
		}
	}
	return chip, phy
}

// ascendModel is the chip model a name column holds, without the "Ascend"
// some releases glue to it ("Ascend910B4" and "910B4" are both 910B4).
func ascendModel(name string) string {
	const brand = "ascend"
	if len(name) > len(brand) && strings.EqualFold(name[:len(brand)], brand) {
		return name[len(brand):]
	}
	return name
}

// ascendName is the chip name column, the one after the NPU id; a bare
// "Ascend" in front of the model is skipped. Only that column is read, so a
// number further along the row (an HBM usage of 3105 MB) is never taken for
// a chip name.
func ascendName(fields []string) string {
	for _, f := range fields[1:min(len(fields), 3)] {
		v := strings.Trim(f, ",;")
		if strings.EqualFold(v, "ascend") {
			continue
		}
		l := strings.ToLower(v)
		if strings.Contains(l, "910") || strings.Contains(l, "310") || strings.Contains(l, "950") || strings.Contains(l, "ascend") {
			return v
		}
		return ""
	}
	return ""
}

// enrichAscendHealthDetails adds the expensive per-device diagnosis only for
// chips that the summary has already flagged. A failed detail query must not
// discard the useful summary or fail the whole hardware probe.
func enrichAscendHealthDetails(ctx context.Context, chips []ascendChip, runner ascendCommandRunner) {
	for i := range chips {
		gpu := &chips[i].gpu
		if gpu.HealthCode == "" {
			continue
		}
		out, err := runner(ctx, "info", "-t", "health", "-i", strconv.Itoa(chips[i].npu), "-c", strconv.Itoa(chips[i].chip))
		if IsCLIUnresponsive(err) || ctx.Err() != nil {
			return // the remaining chips would each wait out the same stall
		}
		if err != nil {
			continue
		}
		status, code, message := parseAscendHealthDetail(out)
		if status == "" && code == "" && message == "" {
			continue
		}
		evidence := accelerator.ParseHealthEvidence(gpu.HealthMessage)
		evidence = append(evidence,
			accelerator.HealthField{Name: "Health Status", Value: status},
			accelerator.HealthField{Name: "Error Code", Value: code},
			accelerator.HealthField{Name: "Error Information", Value: message})
		// The per-chip query can only make the finding worse, never better:
		// the table already stated a verdict for this chip.
		SetCardFinding(gpu, accelerator.MostSevereHealth(gpu.HealthCode, ascendHealthCode(status)), evidence)
	}
}

// ascendEvidence names the chip as npu-smi addresses it, so the health view
// can be asked about that chip, and the summary's verdict.
func ascendEvidence(npu, chip int, health string) []accelerator.HealthField {
	return []accelerator.HealthField{
		{Name: "NPU", Value: strconv.Itoa(npu)},
		{Name: "Chip", Value: strconv.Itoa(chip)},
		{Name: "Health", Value: health},
	}
}

// ascendChipAddress is how npu-smi addresses the chip a device index names.
// An index the table does not list is taken as a one-chip card's NPU id.
func ascendChipAddress(out string, index int) (npu, chip int) {
	for _, c := range parseAscendChips(out) {
		if c.gpu.Index == index {
			return c.npu, c.chip
		}
	}
	return index, 0
}

func parseAscendMetrics(out string) []Metric {
	var metrics []Metric
	for _, c := range parseAscendChips(out) {
		m := Metric{
			Index:  c.gpu.Index,
			UUID:   c.gpu.UUID,
			Vendor: accelerator.VendorAscend,
			Values: map[string]float64{},
		}
		row, line := c.row, c.first
		fields := strings.Fields(strings.ReplaceAll(line, "|", " "))
		if used, total, ok := parseSlashBytes(row); ok {
			m.Values["gpuworks_gpu_memory_used_bytes"] = used
			m.Values["gpuworks_gpu_memory_total_bytes"] = total
		} else if used, total, ok := parseSlashBytesWithDefaultUnit(row, 1<<20); ok {
			m.Values["gpuworks_gpu_memory_used_bytes"] = used
			m.Values["gpuworks_gpu_memory_total_bytes"] = total
		} else if total := ascendMemoryBytes(fields); total > 0 {
			m.Values["gpuworks_gpu_memory_total_bytes"] = float64(total)
		}
		if v, ok := firstRegexFloat(percentRE, row); ok {
			m.Values["gpuworks_gpu_utilization"] = v
		} else if v, ok := ascendContinuationAICore(row); ok {
			m.Values["gpuworks_gpu_utilization"] = v
		}
		if v, ok := firstRegexFloat(wattsRE, row); ok {
			m.Values["gpuworks_gpu_power_watts"] = v
		} else if v, ok := ascendColumnValue(line, "Power(W)"); ok {
			m.Values["gpuworks_gpu_power_watts"] = v
		}
		if v, ok := firstRegexFloat(celsiusRE, row); ok {
			m.Values["gpuworks_gpu_temperature_celsius"] = v
		} else if v, ok := ascendColumnValue(line, "Temp(C)"); ok {
			m.Values["gpuworks_gpu_temperature_celsius"] = v
		}
		if len(m.Values) > 0 {
			metrics = append(metrics, m)
		}
	}
	return metrics
}
