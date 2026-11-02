package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func vendorConfig(id int, info deviceInfo) string {
	return fmt.Sprintf("PciBusId: %s\ncu_mask: 0x0000000000007fff\ncu_mask: 0x0000000000000000\ncu_count: %d\nmem: %d MiB\ndevice_id: %d\nvdev_id: %d\npipe_id: %d\nenable: 1\n", info.PCIBDF, info.CUCount, info.MemoryMiB, info.GPUIndex, id, id)
}

func createPaths(t *testing.T) (func(int) string, string) {
	t.Helper()
	configDir, stateDir := t.TempDir(), t.TempDir()
	return func(id int) string { return filepath.Join(configDir, fmt.Sprintf("vdev%d.conf", id)) }, filepath.Join(stateDir, "vdev-id")
}

func createArgs(recordPath string) []string {
	return []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", recordPath, "--pci-bdf", expectedInfo.PCIBDF}
}

func TestCreateRejectsUnverifiedPhysicalPCIBeforeNativeCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		pci  string
		err  error
	}{
		{"stale index", "0000:02:00.0", nil},
		{"empty physical PCI", "", nil},
		{"physical query failure", "", errors.New("driver unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, record := createPaths(t)
			api := &fakeAPI{ids: []int{17}, physicalPCI: tc.pci, physicalErr: tc.err, info: expectedInfo, exists: true}
			api.createFn = func() {
				if err := os.WriteFile(config(17), []byte(vendorConfig(17, expectedInfo)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			err := runWithConfig(createArgs(record), api, &out, os.Remove, config)
			if err == nil || api.created || api.destroyed != 0 || out.Len() != 0 {
				t.Fatalf("unverified physical selection: error=%v created=%t destroyed=%d output=%q", err, api.created, api.destroyed, out.String())
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("physical query error = %v, want %v", err, tc.err)
			}
			if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified selection wrote ownership: %v", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(record), "vdev-create-pending")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preflight failure wrote create intent: %v", err)
			}
		})
	}
}

func TestCreateVerifiesNormallyReturnedIDBeforeOwnership(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configInfo deviceInfo
		liveInfo   deviceInfo
		exists     bool
		inspectErr error
		preexists  bool
		returnedID int
		newIDs     []int
	}{
		{"wrong config PCI", deviceInfo{GPUIndex: 0, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:02:00.0"}, expectedInfo, true, nil, false, 17, []int{17}},
		{"wrong live PCI", expectedInfo, deviceInfo{GPUIndex: 0, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:02:00.0"}, true, nil, false, 17, []int{17}},
		{"live device absent", expectedInfo, expectedInfo, false, nil, false, 17, []int{17}},
		{"live query failed", expectedInfo, expectedInfo, true, errors.New("driver unavailable"), false, 17, []int{17}},
		{"preexisting ID", expectedInfo, expectedInfo, true, nil, true, 17, nil},
		{"returned different ID", expectedInfo, expectedInfo, true, nil, false, 18, []int{17}},
		{"multiple new configs", expectedInfo, expectedInfo, true, nil, false, 17, []int{17, 18}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, record := createPaths(t)
			api := &fakeAPI{ids: []int{tc.returnedID}, physicalPCI: expectedInfo.PCIBDF, info: tc.liveInfo, exists: tc.exists, inspectErr: tc.inspectErr}
			if tc.preexists {
				if err := os.WriteFile(config(tc.returnedID), []byte(vendorConfig(tc.returnedID, tc.configInfo)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			api.createFn = func() {
				for _, id := range tc.newIDs {
					if err := os.WriteFile(config(id), []byte(vendorConfig(id, tc.configInfo)), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			var out bytes.Buffer
			err := runWithConfig(createArgs(record), api, &out, os.Remove, config)
			if err == nil || !strings.Contains(err.Error(), "manual inspection") || out.Len() != 0 || api.destroyed != 0 || api.stopped != 0 {
				t.Fatalf("unverified creation: error=%v output=%q calls=%v", err, out.String(), api.calls)
			}
			if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unverified creation wrote ownership: %v", err)
			}
		})
	}
}
