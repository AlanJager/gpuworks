package gpu

import (
	"path/filepath"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// A Hygon card's health, read from the driver's own RAS files in sysfs.
//
// hy-smi --showrasinfo and --showpagesinfo print these same files, so reading
// them directly needs no process start and cannot hang on a stuck tool:
//
//	/sys/bus/pci/devices/<bdf>/ras/<block>_err_count  "ue: N\nce: M" since the driver loaded
//	/sys/bus/pci/devices/<bdf>/ras/gpu_vram_bad_pages "0x<pfn> : 0x<size> : R|P|F" per page
//
// The verdict follows what each one means, the way the other vendors' fields
// are read: an uncorrectable error in memory since the driver loaded is a
// memory fault and in any other block a hardware fault; a page waiting to be
// retired needs a reset; a page that could not be retired is a remap failure;
// pages already retired are history. Correctable counts are not findings.
// A card whose driver has no RAS files has no evidence either way, and keeps
// the verdict hy-smi listing it implies.

// hygonHealth reads one card's RAS files and returns its finding.
func hygonHealth(bdf string, read func(string) ([]byte, error)) (string, []accelerator.HealthField) {
	if bdf == "" || read == nil {
		return "", nil
	}
	dir := filepath.Join("/sys/bus/pci/devices", bdf, "ras")
	var evidence []accelerator.HealthField
	memoryUE, otherUE := false, false
	for _, block := range accelerator.HygonRASBlocks {
		raw, err := read(filepath.Join(dir, block+"_err_count"))
		if err != nil {
			continue
		}
		ue, ce := parseHygonErrCount(string(raw))
		if ue > 0 || ce > 0 {
			evidence = append(evidence,
				accelerator.HealthField{Name: "ras." + block + ".ue", Value: strconv.FormatInt(ue, 10)},
				accelerator.HealthField{Name: "ras." + block + ".ce", Value: strconv.FormatInt(ce, 10)})
		}
		if ue > 0 {
			if block == "umc" {
				memoryUE = true
			} else {
				otherUE = true
			}
		}
	}
	retired, pending, failed := 0, 0, 0
	if raw, err := read(filepath.Join(dir, "gpu_vram_bad_pages")); err == nil {
		retired, pending, failed = countHygonBadPages(string(raw))
		if retired+pending+failed > 0 {
			evidence = append(evidence,
				accelerator.HealthField{Name: "bad_pages.retired", Value: strconv.Itoa(retired)},
				accelerator.HealthField{Name: "bad_pages.pending", Value: strconv.Itoa(pending)},
				accelerator.HealthField{Name: "bad_pages.failed", Value: strconv.Itoa(failed)})
		}
	}
	code := ""
	switch {
	case memoryUE:
		code = accelerator.HealthMemoryUncorrectable
	case failed > 0:
		code = accelerator.HealthMemoryRemapFailed
	case otherUE:
		code = accelerator.HealthVendorFault
	case pending > 0:
		code = accelerator.HealthMemoryRepairPending
	case retired > 0:
		code = accelerator.HealthMemoryErrorHistory
	}
	if code == "" {
		return "", nil
	}
	return code, evidence
}

// parseHygonErrCount reads "ue: N" and "ce: M" from a RAS count file.
func parseHygonErrCount(text string) (ue, ce int64) {
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ue":
			ue = n
		case "ce":
			ce = n
		}
	}
	return ue, ce
}

// countHygonBadPages counts the bad page table's rows by their last column:
// R reserved (retired), P pending retirement, F failed to retire.
func countHygonBadPages(text string) (retired, pending, failed int) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		switch strings.TrimSpace(fields[len(fields)-1]) {
		case "R":
			retired++
		case "P":
			pending++
		case "F":
			failed++
		}
	}
	return retired, pending, failed
}
