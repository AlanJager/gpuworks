package accelerator

import (
	"slices"
	"strconv"
	"strings"
)

// Evidence field names, spelled as the vendor tool's query interface spells
// them. The agent queries these fields and records them; the probe command a
// person is shown queries exactly the same fields.
var (
	PPUHealthMemoryFields = []string{
		"ecc.errors.uncorrected.volatile.dram",
		"ecc.errors.uncorrected.aggregate.dram",
		"retired_pages.double_bit.count",
		"retired_pages.pending",
		"remapped_rows.uncorrectable",
		"remapped_rows.pending",
		"remapped_rows.failure",
	}
	PPUHealthXIDFields = []string{
		"xid.correctable.ppu_reset",
		"xid.correctable.os_reboot",
		"xid.correctable.cold_reboot",
	}
	NVIDIAHealthMemoryFields = []string{
		"ecc.errors.uncorrected.volatile.total",
		"ecc.errors.uncorrected.aggregate.total",
		"retired_pages.pending",
		"remapped_rows.pending",
		"remapped_rows.failure",
	}
	// AscendHealthFieldNames are the labels npu-smi prints for the chip address and
	// in its health view ("npu-smi info -t health").
	AscendHealthFieldNames = []string{"NPU", "Chip", "Health", "Health Status", "Error Code", "Error Information"}
)

// HealthGuide is what a person runs for a finding: Probe reproduces the evidence
// fields exactly as the agent read them, Recheck is the vendor tool's readable
// view of the same state, Actions are the recovery commands the vendor tool
// itself names for the finding. A finding whose recovery is not a command
// (reboot the host, contact the hardware vendor) has no Actions; the client
// states it in prose.
type HealthGuide struct {
	Probe   string   `json:"probe"`
	Recheck string   `json:"recheck,omitempty"`
	Actions []string `json:"actions,omitempty"`
}

// HealthGuideFor returns the commands for a card's finding. target addresses the
// card for the vendor tool: its PCI address, or its index when the address is
// unknown. ok is false when there is nothing reliable to show (no finding, an
// unknown code, or a vendor without a health interface).
func HealthGuideFor(vendor, code, target string, evidence []HealthField) (HealthGuide, bool) {
	if code == "" || target == "" {
		return HealthGuide{}, false
	}
	switch vendor {
	case VendorPPU:
		return ppuHealthGuide(code, target, evidence)
	case VendorNVIDIA:
		return nvidiaHealthGuide(code, target, evidence)
	case VendorAscend:
		return ascendHealthGuide(code, evidence)
	}
	return HealthGuide{}, false
}

// HealthTarget picks the address the vendor tools accept for a card.
func HealthTarget(bdf string, index int) string {
	if bdf = strings.TrimSpace(bdf); bdf != "" {
		return bdf
	}
	return strconv.Itoa(index)
}

// probedFields are the evidence fields the vendor query can reproduce: the
// fields the agent recorded when they are all among the known ones, otherwise
// the whole group (a row from an older agent recorded a sentence).
func probedFields(evidence []HealthField, known []string) []string {
	var names []string
	for _, f := range evidence {
		if !slices.Contains(known, f.Name) {
			return known
		}
		names = append(names, f.Name)
	}
	if len(names) == 0 {
		return known
	}
	return names
}

func ppuHealthGuide(code, target string, evidence []HealthField) (HealthGuide, bool) {
	query := func(fields []string) string {
		return "ppu-smi --query-ppu=pci.bus_id," + strings.Join(probedFields(evidence, fields), ",") + " --format=csv -i " + target
	}
	memory := HealthGuide{
		Probe:   query(PPUHealthMemoryFields),
		Recheck: "ppu-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target,
	}
	reset := "ppu-smi -r -i " + target
	switch code {
	case HealthMemoryRepairPending, HealthMemoryUncorrectable:
		memory.Actions = []string{reset}
		return memory, true
	case HealthMemoryRemapFailed, HealthMemoryErrorHistory:
		return memory, true
	case HealthResetRequired:
		return HealthGuide{Probe: query(PPUHealthXIDFields), Actions: []string{reset}}, true
	case HealthRebootRequired, HealthPowerCycleRequired:
		return HealthGuide{Probe: query(PPUHealthXIDFields)}, true
	}
	return HealthGuide{}, false
}

func nvidiaHealthGuide(code, target string, evidence []HealthField) (HealthGuide, bool) {
	memory := HealthGuide{
		Probe:   "nvidia-smi --query-gpu=pci.bus_id," + strings.Join(probedFields(evidence, NVIDIAHealthMemoryFields), ",") + " --format=csv -i " + target,
		Recheck: "nvidia-smi -q -d ECC,PAGE_RETIREMENT,ROW_REMAPPER -i " + target,
	}
	switch code {
	case HealthMemoryRepairPending, HealthMemoryUncorrectable:
		memory.Actions = []string{"nvidia-smi -r -i " + target}
		return memory, true
	case HealthMemoryRemapFailed, HealthMemoryErrorHistory:
		return memory, true
	}
	return HealthGuide{}, false
}

func ascendHealthGuide(code string, evidence []HealthField) (HealthGuide, bool) {
	if code != HealthVendorWarning && code != HealthVendorFault {
		return HealthGuide{}, false
	}
	guide := HealthGuide{Probe: "npu-smi info"}
	npu, chip := HealthFieldValue(evidence, "NPU"), HealthFieldValue(evidence, "Chip")
	if npu != "" && chip != "" {
		guide.Recheck = "npu-smi info -t health -i " + npu + " -c " + chip
	}
	return guide, true
}

// ToolCommand is the vendor tool's own overview command: what a person runs
// first when the tool itself fails or a card is missing.
func ToolCommand(vendor string) string {
	switch vendor {
	case VendorNVIDIA:
		return "nvidia-smi"
	case VendorPPU:
		return "ppu-smi"
	case VendorAscend:
		return "npu-smi info"
	case VendorHygon:
		return "hy-smi"
	}
	return ""
}
