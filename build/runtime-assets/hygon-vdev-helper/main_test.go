package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeAPI struct {
	initialized bool
	shutdown    bool
	ids         []int
	createErr   error
	createFn    func()
	created     bool
	physicalPCI string
	physicalErr error
	info        deviceInfo
	exists      bool
	inspectErr  error
	inspectFn   func(int) (deviceInfo, bool, error)
	stopped     int
	destroyed   int
	destroyErr  error
	stopErr     error
	calls       []string
}

func (f *fakeAPI) Init() error     { f.initialized = true; return nil }
func (f *fakeAPI) ShutDown() error { f.shutdown = true; return nil }
func (f *fakeAPI) PhysicalPCIBDF(int) (string, error) {
	f.calls = append(f.calls, "physical-pci")
	return f.physicalPCI, f.physicalErr
}
func (f *fakeAPI) Create(_, _, _ int) ([]int, error) {
	f.created = true
	if f.createFn != nil {
		f.createFn()
	}
	return f.ids, f.createErr
}
func (f *fakeAPI) Stop(id int) error {
	f.stopped = id
	f.calls = append(f.calls, "stop")
	return f.stopErr
}
func (f *fakeAPI) Destroy(id int) error {
	f.destroyed = id
	f.calls = append(f.calls, "destroy")
	return f.destroyErr
}
func (f *fakeAPI) Inspect(id int) (deviceInfo, bool, error) {
	f.calls = append(f.calls, "inspect")
	if f.inspectFn != nil {
		return f.inspectFn(id)
	}
	return f.info, f.exists, f.inspectErr
}

const testOwnerToken = "00000000000000000000000000000001"

var releaseArgs = []string{"release", "--vdev-id", "9", "--owner-token", testOwnerToken, "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--pci-bdf", "0000:01:00.0"}
var expectedInfo = deviceInfo{GPUIndex: 0, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:01:00.0"}

func runRelease(t *testing.T, args []string, api deviceAPI, out *bytes.Buffer, removeConfig func(string) error) error {
	t.Helper()
	dir := t.TempDir()
	hostPath := filepath.Join(dir, "vdev9.conf")
	snapshotPath := filepath.Join(dir, "owned.conf")
	for _, path := range []string{hostPath, snapshotPath} {
		if err := os.WriteFile(path, []byte("owned config\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCurrentOwner(dir, 9, currentOwner{Token: testOwnerToken, Active: true}); err != nil {
		t.Fatal(err)
	}
	return runWithConfig(append(append([]string{}, args...), "--snapshot-path", snapshotPath), api, out, removeConfig, func(int) string { return hostPath })
}

func configFixture(t *testing.T, api *fakeAPI) func(int) string {
	t.Helper()
	dir := t.TempDir()
	path := func(id int) string { return filepath.Join(dir, "vdev"+strconv.Itoa(id)+".conf") }
	api.createFn = func() {
		for _, id := range api.ids {
			if err := os.WriteFile(path(id), []byte(vendorConfig(id, expectedInfo)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return path
}

func TestCreateContract(t *testing.T) {
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, info: expectedInfo, exists: true}
	var out bytes.Buffer
	if err := runWithConfig([]string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536"}, api, &out, os.Remove, configFixture(t, api)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "{\"vdev_id\":17}\n" || !api.initialized || !api.shutdown {
		t.Fatalf("output=%q init=%t shutdown=%t", out.String(), api.initialized, api.shutdown)
	}
}

func TestCreateRejectsAmbiguousIDs(t *testing.T) {
	for _, ids := range [][]int{{}, {17, 18}} {
		api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: ids}
		var out bytes.Buffer
		err := runWithConfig([]string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536"}, api, &out, os.Remove, configFixture(t, api))
		if err == nil || !strings.Contains(err.Error(), "manual inspection") || out.Len() != 0 || !api.shutdown {
			t.Fatalf("ids=%v error=%v output=%q shutdown=%t", ids, err, out.String(), api.shutdown)
		}
	}
}

func TestCreateRecoversOnlyOneVerifiedNewVdev(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ids       []int
		createErr error
		configs   map[string]string
		info      deviceInfo
		exists    bool
		wantError string
	}{
		{name: "lost ID", createErr: errors.New("Failed to get config files"), configs: map[string]string{"vdev17.conf": "vdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"}, info: expectedInfo, exists: true},
		{name: "vendor config with repeated mask words", createErr: errors.New("Failed to get config files"), configs: map[string]string{"vdev17.conf": "PciBusId: 0000:01:00.0\ncu_mask: 0x0000000000007fff\ncu_mask: 0x0000000000000000\ncu_count: 15\nmem: 1536 MiB\ndevice_id: 0\nvdev_id: 17\npipe_id: 17\nenable: 1\n"}, info: expectedInfo, exists: true},
		{name: "empty ID result", configs: map[string]string{"vdev17.conf": "vdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"}, info: expectedInfo, exists: true},
		{name: "duplicate identity field", configs: map[string]string{"vdev17.conf": "vdev_id: 17\nvdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"}, wantError: "duplicate vdev_id"},
		{name: "no new config", createErr: errors.New("Failed to get config files"), wantError: "no new vdev config"},
		{name: "multiple reported IDs", ids: []int{17, 18}, configs: map[string]string{"vdev17.conf": "vdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"}, info: expectedInfo, exists: true, wantError: "ambiguous vdev IDs"},
		{name: "two new configs", configs: map[string]string{"vdev17.conf": "", "vdev18.conf": ""}, wantError: "multiple new vdev configs"},
		{name: "wrong config identity", configs: map[string]string{"vdev17.conf": "vdev_id: 17\ncu_count: 14\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"}, info: expectedInfo, exists: true, wantError: "does not match"},
		{name: "noncanonical filename", configs: map[string]string{"vdev017.conf": "vdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"}, info: expectedInfo, exists: true, wantError: "invalid new vdev config name"},
		{name: "wrong live identity", configs: map[string]string{"vdev17.conf": "vdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"}, info: deviceInfo{GPUIndex: 1, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:01:00.0"}, exists: true, wantError: "live identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configDir, stateDir := t.TempDir(), t.TempDir()
			api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: tc.ids, createErr: tc.createErr, info: tc.info, exists: tc.exists}
			api.createFn = func() {
				for name, data := range tc.configs {
					if err := os.WriteFile(filepath.Join(configDir, name), []byte(data), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			var out bytes.Buffer
			args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", filepath.Join(stateDir, "vdev-id"), "--pci-bdf", "0000:01:00.0"}
			err := runWithConfig(args, api, &out, os.Remove, func(id int) string { return filepath.Join(configDir, "vdev"+strconv.Itoa(id)+".conf") })
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || out.Len() != 0 || api.destroyed != 0 {
					t.Fatalf("error=%v output=%q destroyed=%d", err, out.String(), api.destroyed)
				}
				if _, err := os.Stat(filepath.Join(stateDir, "vdev-id")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unexpected ownership record: %v", err)
				}
				return
			}
			if err != nil || out.String() != "{\"vdev_id\":17}\n" {
				t.Fatalf("error=%v output=%q", err, out.String())
			}
			if _, err := os.Stat(filepath.Join(stateDir, "vdev-id")); err != nil {
				t.Fatalf("ownership record missing after recovery: %v", err)
			}
		})
	}
}

func TestStandaloneCreateDerivesPCIAndVerifiesUnreportedID(t *testing.T) {
	dir := t.TempDir()
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, exists: true, info: expectedInfo}
	api.createFn = func() {
		if err := os.WriteFile(filepath.Join(dir, "vdev17.conf"), []byte("vdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	err := runWithConfig([]string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536"}, api, &out, os.Remove, func(id int) string { return filepath.Join(dir, "vdev"+strconv.Itoa(id)+".conf") })
	if err != nil || out.String() != "{\"vdev_id\":17}\n" || api.destroyed != 0 || strings.Join(api.calls, ",") != "physical-pci,inspect" {
		t.Fatalf("error=%v output=%q destroyed=%d", err, out.String(), api.destroyed)
	}
}

func TestCreateDoesNotAdoptSymlinkedConfig(t *testing.T) {
	configDir, stateDir := t.TempDir(), t.TempDir()
	target := filepath.Join(stateDir, "external.conf")
	if err := os.WriteFile(target, []byte("vdev_id: 17\ncu_count: 15\nmem: 1536 MiB\npcibusid: 0000:01:00.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, exists: true, info: expectedInfo}
	api.createFn = func() {
		if err := os.Symlink(target, filepath.Join(configDir, "vdev17.conf")); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", filepath.Join(stateDir, "vdev-id"), "--pci-bdf", "0000:01:00.0"}
	err := runWithConfig(args, api, &out, os.Remove, func(id int) string { return filepath.Join(configDir, "vdev"+strconv.Itoa(id)+".conf") })
	if err == nil || !strings.Contains(err.Error(), "not a regular file") || out.Len() != 0 || api.destroyed != 0 {
		t.Fatalf("error=%v output=%q destroyed=%d", err, out.String(), api.destroyed)
	}
}

func TestCreateRefusesStaleOrUnverifiableConfigBeforeNativeCall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		file       string
		inspectErr error
		want       string
	}{
		{name: "absent device", file: "vdev42.conf", want: "no live device"},
		{name: "inspect error", file: "vdev42.conf", inspectErr: errors.New("driver unavailable"), want: "driver unavailable"},
		{name: "malformed id", file: "vdevunknown.conf", want: "invalid ID"},
		{name: "noncanonical id", file: "vdev042.conf", want: "invalid ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte("stale"), 0o644); err != nil {
				t.Fatal(err)
			}
			api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, inspectErr: tc.inspectErr}
			var out bytes.Buffer
			args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536"}
			err := runWithConfig(args, api, &out, os.Remove, func(int) string { return filepath.Join(dir, "vdev17.conf") })
			if err == nil || !strings.Contains(err.Error(), tc.want) || api.created || out.Len() != 0 {
				t.Fatalf("error=%v created=%t output=%q", err, api.created, out.String())
			}
		})
	}
}

func TestCreateAllowsConfigForLiveDevice(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "vdev42.conf"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, exists: true, info: expectedInfo}
	api.createFn = func() {
		if err := os.WriteFile(filepath.Join(dir, "vdev17.conf"), []byte(vendorConfig(17, expectedInfo)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536"}
	if err := runWithConfig(args, api, &out, os.Remove, func(int) string { return filepath.Join(dir, "vdev17.conf") }); err != nil || !api.created || out.String() != "{\"vdev_id\":17}\n" {
		t.Fatalf("error=%v created=%t output=%q", err, api.created, out.String())
	}
}

func TestVdevLockSerializesOperations(t *testing.T) {
	dir := t.TempDir()
	first, err := lockVdevDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if first != nil {
			unlockVdevDir(first)
		}
	}()
	acquired := make(chan error, 1)
	go func() {
		second, err := lockVdevDir(dir)
		if err == nil {
			err = unlockVdevDir(second)
		}
		acquired <- err
	}()
	select {
	case err := <-acquired:
		t.Fatalf("second operation acquired lock before release: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := unlockVdevDir(first); err != nil {
		t.Fatal(err)
	}
	first = nil
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("second operation did not acquire released lock")
	}
}

func TestCreateRecordsIdentityBeforeReturningID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vdev-id")
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, info: expectedInfo, exists: true}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", path, "--pci-bdf", "0000:01:00.0"}
	config := configFixture(t, api)
	if err := runWithConfig(args, api, &out, os.Remove, config); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(filepath.Join(filepath.Dir(path), "vdev17.conf"))
	if err != nil || string(snapshot) != vendorConfig(17, expectedInfo) {
		t.Fatalf("snapshot=%q error=%v", snapshot, err)
	}
	data, err := os.ReadFile(path)
	var record ownershipRecord
	if err != nil || json.Unmarshal(data, &record) != nil || len(record.OwnerToken) != 32 || record.VDevID != 17 || record.GPUIndex != 0 || record.CUCount != 15 || record.MemoryMiB != 1536 || record.PCIBDF != "0000:01:00.0" || out.String() != "{\"vdev_id\":17}\n" {
		t.Fatalf("read error=%v record=%q output=%q", err, data, out.String())
	}
	owner, err := readCurrentOwner(filepath.Dir(config(0)), 17)
	if err != nil || owner.Token != record.OwnerToken || !owner.Active {
		t.Fatalf("current owner=%+v error=%v", owner, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stat error=%v info=%v", err, info)
	}
	snapshotInfo, err := os.Stat(filepath.Join(filepath.Dir(path), "vdev17.conf"))
	if err != nil || snapshotInfo.Mode().Perm() != 0o644 {
		t.Fatalf("snapshot stat error=%v info=%v", err, snapshotInfo)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || dirInfo.Mode().Perm() != 0o755 {
		t.Fatalf("parent stat error=%v info=%v", err, dirInfo)
	}
}

func TestCreateDoesNotReplaceExistingSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vdev-id")
	snapshotPath := filepath.Join(dir, "vdev17.conf")
	if err := os.WriteFile(snapshotPath, []byte("prior owner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, exists: true, info: expectedInfo}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", path, "--pci-bdf", "0000:01:00.0"}
	err := runWithConfig(args, api, &out, func(string) error { return os.ErrNotExist }, configFixture(t, api))
	data, readErr := os.ReadFile(snapshotPath)
	if err == nil || !strings.Contains(err.Error(), "snapshot vdev 17 config") || readErr != nil || string(data) != "prior owner\n" || out.Len() != 0 || api.destroyed != 17 {
		t.Fatalf("error=%v snapshot=%q read error=%v output=%q calls=%v", err, data, readErr, out.String(), api.calls)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected ownership record: %v", err)
	}
}

func TestCreateDoesNotReplaceExistingRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vdev-id")
	if err := os.WriteFile(path, []byte("prior owner\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, exists: true, info: expectedInfo}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", path, "--pci-bdf", "0000:01:00.0"}
	err := runWithConfig(args, api, &out, func(string) error { return os.ErrNotExist }, configFixture(t, api))
	data, readErr := os.ReadFile(path)
	_, snapshotErr := os.Stat(filepath.Join(dir, "vdev17.conf"))
	if err == nil || !strings.Contains(err.Error(), "ownership record already exists") || readErr != nil || string(data) != "prior owner\n" || !errors.Is(snapshotErr, os.ErrNotExist) || out.Len() != 0 || api.created || api.destroyed != 0 {
		t.Fatalf("error=%v record=%q read error=%v snapshot error=%v output=%q calls=%v", err, data, readErr, snapshotErr, out.String(), api.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "vdev-create-pending")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("existing-record preflight wrote create intent: %v", err)
	}
}

func TestCreateRecordFailureRollsBackVerifiedDevice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vdev-id")
	if err := os.WriteFile(filepath.Join(dir, "vdev17.conf"), []byte("prior snapshot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, exists: true, info: expectedInfo}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", path, "--pci-bdf", "0000:01:00.0"}
	err := runWithConfig(args, api, &out, func(string) error { return os.ErrNotExist }, configFixture(t, api))
	if err == nil || !strings.Contains(err.Error(), "snapshot vdev 17 config") || out.Len() != 0 || api.stopped != 17 || api.destroyed != 17 || strings.Join(api.calls, ",") != "physical-pci,inspect,inspect,stop,destroy" {
		t.Fatalf("error=%v output=%q calls=%v", err, out.String(), api.calls)
	}
}

func TestCreateRecordFailureReportsUnsafeRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vdev-id")
	if err := os.WriteFile(filepath.Join(dir, "vdev17.conf"), []byte("prior snapshot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, exists: true, info: deviceInfo{GPUIndex: 1, CUCount: 15, MemoryMiB: 1536, PCIBDF: "0000:01:00.0"}}
	inspections := 0
	api.inspectFn = func(int) (deviceInfo, bool, error) {
		inspections++
		if inspections == 1 {
			return expectedInfo, true, nil
		}
		return api.info, true, nil
	}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", path, "--pci-bdf", "0000:01:00.0"}
	err := runWithConfig(args, api, &out, func(string) error { return os.ErrNotExist }, configFixture(t, api))
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") || !strings.Contains(err.Error(), "vdev 17") || api.stopped != 0 || api.destroyed != 0 || out.Len() != 0 {
		t.Fatalf("error=%v calls=%v output=%q", err, api.calls, out.String())
	}
}

func TestCreateRecordFailureReportsDestroyError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vdev-id")
	if err := os.WriteFile(filepath.Join(dir, "vdev17.conf"), []byte("prior snapshot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, exists: true, info: expectedInfo, destroyErr: errors.New("device busy")}
	var out bytes.Buffer
	args := []string{"create", "--gpu-index", "0", "--cu-count", "15", "--memory-mib", "1536", "--record-path", path, "--pci-bdf", "0000:01:00.0"}
	err := runWithConfig(args, api, &out, func(string) error { return os.ErrNotExist }, configFixture(t, api))
	if err == nil || !strings.Contains(err.Error(), "snapshot vdev 17 config") || !strings.Contains(err.Error(), "device busy") || out.Len() != 0 {
		t.Fatalf("error=%v calls=%v output=%q", err, api.calls, out.String())
	}
}
