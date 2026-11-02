package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The upstream binding derives IDs from directory scans, which may return a
// wrong ID or lose the ID of an already-live device. Verify every creation
// against one newly named config whose file and live DCGM identity agree.
func recoverCreatedVdevID(api deviceAPI, before map[string]struct{}, dir string, expected deviceInfo) (int, error) {
	if expected.PCIBDF == "" {
		return 0, errors.New("PCI address is required to verify a newly created vdev")
	}
	after, err := vdevConfigNames(dir)
	if err != nil {
		return 0, fmt.Errorf("scan configs after create: %w", err)
	}
	var candidate string
	for name := range after {
		if _, existed := before[name]; existed {
			continue
		}
		if candidate != "" {
			return 0, errors.New("multiple new vdev configs")
		}
		candidate = name
	}
	if candidate == "" {
		return 0, errors.New("no new vdev config")
	}
	id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(candidate, "vdev"), ".conf"))
	if err != nil || id < 0 || candidate != fmt.Sprintf("vdev%d.conf", id) {
		return 0, fmt.Errorf("invalid new vdev config name %q", candidate)
	}
	path := filepath.Join(dir, candidate)
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return 0, fmt.Errorf("stat new vdev config %q: %w", candidate, err)
	}
	if !fileInfo.Mode().IsRegular() {
		return 0, fmt.Errorf("new vdev config %q is not a regular file", candidate)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read new vdev config %q: %w", candidate, err)
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		// The vendor writes one cu_mask line per mask word. Only fields used
		// for identity checks must be unique.
		switch key {
		case "vdev_id", "cu_count", "mem", "pcibusid":
		default:
			continue
		}
		if _, duplicate := fields[key]; duplicate {
			return 0, fmt.Errorf("duplicate %s in new vdev config %q", key, candidate)
		}
		fields[key] = strings.TrimSpace(value)
	}
	if fields["vdev_id"] != strconv.Itoa(id) || fields["cu_count"] != strconv.Itoa(expected.CUCount) || fields["mem"] != fmt.Sprintf("%d MiB", expected.MemoryMiB) || !strings.EqualFold(fields["pcibusid"], expected.PCIBDF) {
		return 0, fmt.Errorf("new vdev config %q does not match the requested ID, CU, memory or PCI address", candidate)
	}
	info, exists, err := api.Inspect(id)
	if err != nil {
		return 0, fmt.Errorf("inspect new vdev %d: %w", id, err)
	}
	if !exists || info.GPUIndex != expected.GPUIndex || info.CUCount != expected.CUCount || info.MemoryMiB != expected.MemoryMiB || !strings.EqualFold(info.PCIBDF, expected.PCIBDF) {
		return 0, fmt.Errorf("new vdev %d is absent or its live identity does not match the request", id)
	}
	return id, nil
}
