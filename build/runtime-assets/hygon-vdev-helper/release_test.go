package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectContractAndError(t *testing.T) {
	api := &fakeAPI{exists: true, info: expectedInfo}
	var out bytes.Buffer
	if err := run([]string{"inspect", "--vdev-id", "9"}, api, &out); err != nil || out.String() != "{\"vdev_id\":9,\"exists\":true,\"gpu_index\":0,\"cu_count\":15,\"memory_mib\":1536,\"pci_bdf\":\"0000:01:00.0\"}\n" {
		t.Fatalf("error=%v output=%q", err, out.String())
	}
	api.inspectErr = errors.New("driver unavailable")
	out.Reset()
	if err := run([]string{"inspect", "--vdev-id", "9"}, api, &out); err == nil || out.Len() != 0 {
		t.Fatalf("error=%v output=%q", err, out.String())
	}
}

func TestReleaseDoesNotDestroyOnInspectionError(t *testing.T) {
	api := &fakeAPI{inspectErr: errors.New("driver unavailable")}
	var out bytes.Buffer
	if err := runRelease(t, releaseArgs, api, &out, os.Remove); err == nil || api.stopped != 0 || api.destroyed != 0 {
		t.Fatalf("error=%v stopped=%d destroyed=%d", err, api.stopped, api.destroyed)
	}
}

func TestReleaseDoesNotDestroyReusedID(t *testing.T) {
	for _, info := range []deviceInfo{
		{GPUIndex: 1, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:01:00.0"},
		{GPUIndex: 0, CUCount: 16, MemoryMiB: 1536, PCIBDF: "0000:01:00.0"},
		{GPUIndex: 0, CUCount: 15, MemoryMiB: 2048, PCIBDF: "0000:01:00.0"},
		{GPUIndex: 0, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:02:00.0"},
	} {
		api := &fakeAPI{exists: true, info: info}
		var out bytes.Buffer
		removed := false
		err := runRelease(t, releaseArgs, api, &out, func(string) error { removed = true; return nil })
		if err == nil || !strings.Contains(err.Error(), "identity mismatch") || api.stopped != 0 || api.destroyed != 0 || removed || out.Len() != 0 || strings.Join(api.calls, ",") != "inspect" {
			t.Fatalf("info=%+v error=%v calls=%v removed=%t output=%q", info, err, api.calls, removed, out.String())
		}
	}
}

func TestReleaseRequiresMatchingOwnedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		host        string
		snapshot    string
		missingHost bool
		missingSnap bool
		want        string
	}{
		{name: "changed host", host: "replacement", snapshot: "owned", want: "differs from owned snapshot"},
		{name: "missing host", missingHost: true, snapshot: "owned", want: "read vdev 9 host config"},
		{name: "missing snapshot", host: "owned", missingSnap: true, want: "read vdev 9 config snapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			hostPath := filepath.Join(dir, "vdev9.conf")
			if err := writeCurrentOwner(dir, 9, currentOwner{Token: testOwnerToken, Active: true}); err != nil {
				t.Fatal(err)
			}
			snapshotPath := filepath.Join(dir, "owned.conf")
			if !tc.missingHost {
				if err := os.WriteFile(hostPath, []byte(tc.host), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.missingSnap {
				if err := os.WriteFile(snapshotPath, []byte(tc.snapshot), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			api := &fakeAPI{exists: true, info: expectedInfo}
			var out bytes.Buffer
			removed := false
			args := append(append([]string{}, releaseArgs...), "--snapshot-path", snapshotPath)
			err := runWithConfig(args, api, &out, func(string) error { removed = true; return nil }, func(int) string { return hostPath })
			if err == nil || !strings.Contains(err.Error(), tc.want) || removed || api.stopped != 0 || api.destroyed != 0 || out.Len() != 0 {
				t.Fatalf("error=%v removed=%t calls=%v output=%q", err, removed, api.calls, out.String())
			}
		})
	}
}

func TestReleaseStopsBeforeSingleDeviceDestroy(t *testing.T) {
	api := &fakeAPI{exists: true, info: expectedInfo, stopErr: errors.New("already stopped"), destroyErr: errors.New("device busy")}
	var out bytes.Buffer
	err := runRelease(t, releaseArgs, api, &out, os.Remove)
	if err == nil || !strings.Contains(err.Error(), "already stopped") || !strings.Contains(err.Error(), "device busy") || api.stopped != 9 || api.destroyed != 9 || strings.Join(api.calls, ",") != "inspect,stop,destroy" || out.Len() != 0 {
		t.Fatalf("error=%v stopped=%d destroyed=%d calls=%v output=%q", err, api.stopped, api.destroyed, api.calls, out.String())
	}
}

func TestReleaseAcceptsEquivalentBDFCase(t *testing.T) {
	api := &fakeAPI{exists: true, info: deviceInfo{GPUIndex: 0, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:01:00.A"}}
	var out bytes.Buffer
	args := []string{"release", "--vdev-id", "9", "--owner-token", testOwnerToken, "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--pci-bdf", "0000:01:00.a"}
	err := runRelease(t, args, api, &out, func(string) error { return os.ErrNotExist })
	if err != nil || strings.Join(api.calls, ",") != "inspect,stop,destroy" || out.String() != "{\"vdev_id\":9}\n" {
		t.Fatalf("error=%v calls=%v output=%q", err, api.calls, out.String())
	}
}

func TestReleaseReportsStopErrorAfterSuccessfulDestroy(t *testing.T) {
	api := &fakeAPI{exists: true, info: expectedInfo, stopErr: errors.New("already stopped")}
	var out bytes.Buffer
	removed := ""
	err := runRelease(t, releaseArgs, api, &out, func(path string) error {
		removed = path
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "already stopped") || filepath.Base(removed) != "vdev9.conf" || out.Len() != 0 {
		t.Fatalf("error=%v removed=%q output=%q", err, removed, out.String())
	}
}

func TestReleaseAbsentDeviceIsIdempotent(t *testing.T) {
	api := &fakeAPI{exists: false}
	var out bytes.Buffer
	removed := false
	err := runRelease(t, releaseArgs, api, &out, func(path string) error {
		removed = true
		return os.ErrNotExist
	})
	if err == nil || !strings.Contains(err.Error(), "use prune") || removed || api.stopped != 0 || api.destroyed != 0 || out.Len() != 0 {
		t.Fatalf("error=%v removed=%t stopped=%d destroyed=%d output=%q", err, removed, api.stopped, api.destroyed, out.String())
	}
}

func TestRecreatedIdenticalIDRejectsPreviousOwner(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "vdev9.conf")
	api := &fakeAPI{ids: []int{9}, info: expectedInfo}
	api.createFn = func() {
		api.exists = true
		if err := os.WriteFile(config, []byte("identical vendor config\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	configPath := func(int) string { return config }
	create := func(name string) ownershipRecord {
		t.Helper()
		stateDir := filepath.Join(dir, name)
		if err := os.Mkdir(stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(stateDir, "vdev-id")
		args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", path, "--pci-bdf", expectedInfo.PCIBDF}
		if err := runWithConfig(args, api, &bytes.Buffer{}, os.Remove, configPath); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var record ownershipRecord
		if err := json.Unmarshal(data, &record); err != nil {
			t.Fatal(err)
		}
		return record
	}
	first := create("first")
	firstSnapshot := filepath.Join(dir, "first", "vdev9.conf")
	release := func(record ownershipRecord, snapshot string) error {
		args := append(append([]string{}, releaseArgs...), "--snapshot-path", snapshot)
		args[4] = record.OwnerToken
		return runWithConfig(args, api, &bytes.Buffer{}, os.Remove, configPath)
	}
	if err := release(first, firstSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := runWithConfig([]string{"inspect", "--vdev-id", "9", "--owner-token", first.OwnerToken}, api, &bytes.Buffer{}, os.Remove, configPath); err == nil || !strings.Contains(err.Error(), "owner was released") {
		t.Fatalf("released token accepted a live replacement: %v", err)
	}
	api.exists = false
	second := create("second")
	if first.OwnerToken == second.OwnerToken {
		t.Fatal("new native creation reused the ownership token")
	}
	api.stopped, api.destroyed = 0, 0
	if err := release(first, firstSnapshot); err == nil || !strings.Contains(err.Error(), "different creation") {
		t.Fatalf("stale release error=%v", err)
	}
	if api.stopped != 0 || api.destroyed != 0 {
		t.Fatal("stale release touched the recreated device")
	}
	for _, args := range [][]string{
		{"inspect", "--vdev-id", "9", "--owner-token", first.OwnerToken},
		{"prune", "--vdev-id", "9", "--owner-token", first.OwnerToken, "--snapshot-path", firstSnapshot},
	} {
		if err := runWithConfig(args, api, &bytes.Buffer{}, os.Remove, configPath); err == nil || !strings.Contains(err.Error(), "different creation") {
			t.Fatalf("stale %s error=%v", args[0], err)
		}
	}
	if err := release(second, filepath.Join(dir, "second", "vdev9.conf")); err != nil {
		t.Fatalf("current owner cannot release: %v", err)
	}
}

func TestPruneRemovesOnlyMatchingConfigOfAbsentDevice(t *testing.T) {
	dir := t.TempDir()
	hostPath := filepath.Join(dir, "vdev17.conf")
	snapshotPath := filepath.Join(dir, "owned.conf")
	for _, path := range []string{hostPath, snapshotPath} {
		if err := os.WriteFile(path, []byte("owned config\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	api := &fakeAPI{}
	if err := writeCurrentOwner(dir, 17, currentOwner{Token: testOwnerToken, Active: true}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"prune", "--vdev-id", "17", "--owner-token", testOwnerToken, "--snapshot-path", snapshotPath}
	if err := runWithConfig(args, api, &out, os.Remove, func(int) string { return hostPath }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hostPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host config remains: %v", err)
	}
	if data, err := os.ReadFile(snapshotPath); err != nil || string(data) != "owned config\n" || out.String() != "{\"vdev_id\":17}\n" || strings.Join(api.calls, ",") != "inspect" {
		t.Fatalf("snapshot=%q error=%v output=%q calls=%v", data, err, out.String(), api.calls)
	}
	// A repeated prune still verifies absence and succeeds without a host file.
	out.Reset()
	if err := runWithConfig(args, api, &out, os.Remove, func(int) string { return hostPath }); err != nil || out.String() != "{\"vdev_id\":17}\n" {
		t.Fatalf("repeat error=%v output=%q", err, out.String())
	}
}

func TestPruneFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exists     bool
		inspectErr error
		host       string
		snapshot   string
		removeErr  error
		want       string
	}{
		{name: "live device", exists: true, host: "owned", snapshot: "owned", want: "still exists"},
		{name: "query failure", inspectErr: errors.New("driver unavailable"), host: "owned", snapshot: "owned", want: "driver unavailable"},
		{name: "different config", host: "replacement", snapshot: "owned", want: "differs from owned snapshot"},
		{name: "missing snapshot", host: "owned", want: "read vdev 17 config snapshot"},
		{name: "remove failure", host: "owned", snapshot: "owned", removeErr: errors.New("permission denied"), want: "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := writeCurrentOwner(dir, 17, currentOwner{Token: testOwnerToken, Active: true}); err != nil {
				t.Fatal(err)
			}
			hostPath := filepath.Join(dir, "vdev17.conf")
			snapshotPath := filepath.Join(dir, "owned.conf")
			if err := os.WriteFile(hostPath, []byte(tc.host), 0o644); err != nil {
				t.Fatal(err)
			}
			if tc.snapshot != "" {
				if err := os.WriteFile(snapshotPath, []byte(tc.snapshot), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			api := &fakeAPI{exists: tc.exists, inspectErr: tc.inspectErr}
			var out bytes.Buffer
			removed := false
			args := []string{"prune", "--vdev-id", "17", "--owner-token", testOwnerToken, "--snapshot-path", snapshotPath}
			err := runWithConfig(args, api, &out, func(string) error { removed = true; return tc.removeErr }, func(int) string { return hostPath })
			data, readErr := os.ReadFile(hostPath)
			if err == nil || !strings.Contains(err.Error(), tc.want) || readErr != nil || string(data) != tc.host || out.Len() != 0 || removed != (tc.removeErr != nil) || strings.Join(api.calls, ",") != "inspect" {
				t.Fatalf("error=%v host=%q read error=%v output=%q removed=%t calls=%v", err, data, readErr, out.String(), removed, api.calls)
			}
		})
	}
}

func TestInvalidArgumentsDoNotInitializeDCGM(t *testing.T) {
	for _, args := range [][]string{{}, {"create", "--gpu-index", "-1", "--cu-count", "1", "--memory-mib", "1"}, {"create", "--gpu-index", "0", "--cu-count", "1", "--memory-mib", "1", "--record-path", "/tmp/ledger"}, {"release", "--vdev-id", "-1"}, {"release", "--vdev-id", "9"}, {"inspect", "--vdev-id", "1", "extra"}, {"prune", "--vdev-id", "9"}, {"prune", "--vdev-id", "-1", "--snapshot-path", "/tmp/snapshot"}} {
		api := &fakeAPI{}
		if err := run(args, api, &bytes.Buffer{}); err == nil || api.initialized {
			t.Fatalf("args=%v error=%v initialized=%t", args, err, api.initialized)
		}
	}
}
