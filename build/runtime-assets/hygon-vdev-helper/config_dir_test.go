package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCreateBootstrapsMissingConfigDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vdev")
	config := func(id int) string { return filepath.Join(dir, "vdev"+strconv.Itoa(id)+".conf") }
	record := filepath.Join(t.TempDir(), "vdev-id")
	api := &fakeAPI{physicalPCI: expectedInfo.PCIBDF, ids: []int{17}, info: expectedInfo, exists: true}
	api.createFn = func() {
		if err := os.WriteFile(config(17), []byte(vendorConfig(17, expectedInfo)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := runWithConfig(createArgs(record), api, &out, os.Remove, config); err != nil {
		t.Fatal(err)
	}
	if !api.created || out.String() != "{\"vdev_id\":17}\n" {
		t.Fatalf("created=%t output=%q", api.created, out.String())
	}
	owner, err := readCurrentOwner(dir, 17)
	if err != nil || !owner.Active || owner.Token == "" {
		t.Fatalf("owner=%+v error=%v", owner, err)
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("managed creation did not persist ownership: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(record), createIntentName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed creation retained intent: %v", err)
	}
}

func TestCreateConfigDirectoryFailureStopsBeforeInitialization(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(dir, []byte("preserve me"), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "vdev-id")
	api := &fakeAPI{}
	var out bytes.Buffer
	err := runWithConfig(createArgs(record), api, &out, os.Remove, func(int) string { return filepath.Join(dir, "vdev0.conf") })
	if err == nil || !strings.Contains(err.Error(), "create vdev configuration directory") || api.initialized || api.created || len(api.calls) != 0 || out.Len() != 0 {
		t.Fatalf("error=%v initialized=%t created=%t calls=%v output=%q", err, api.initialized, api.created, api.calls, out.String())
	}
	for _, path := range []string{record, filepath.Join(filepath.Dir(record), createIntentName)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("directory failure wrote managed state %s: %v", path, err)
		}
	}
	if data, err := os.ReadFile(dir); err != nil || string(data) != "preserve me" {
		t.Fatalf("preexisting file changed: %q, %v", data, err)
	}
}

func TestInvalidCreateDoesNotBootstrapConfigDirectory(t *testing.T) {
	for _, args := range [][]string{
		{"create", "--gpu-index", "-1", "--cu-count", "2", "--memory-mib", "1024"},
		{"create", "--gpu-index", "0", "--cu-count", "0", "--memory-mib", "1024"},
		{"create", "--gpu-index", "0", "--cu-count", "2", "--memory-mib", "0"},
		{"create", "--gpu-index", "0", "--cu-count", "2", "--memory-mib", "1024", "--record-path", "/unpaired"},
		{"create", "--unknown"},
	} {
		dir := filepath.Join(t.TempDir(), "vdev")
		api := &fakeAPI{}
		err := runWithConfig(args, api, &bytes.Buffer{}, os.Remove, func(int) string { return filepath.Join(dir, "vdev0.conf") })
		if err == nil || api.initialized || api.created {
			t.Fatalf("args=%v error=%v initialized=%t created=%t", args, err, api.initialized, api.created)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid args %v created config directory: %v", args, err)
		}
	}
}

func TestReleaseDoesNotBootstrapMissingConfigDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vdev")
	api := &fakeAPI{}
	args := append(append([]string{}, releaseArgs...), "--snapshot-path", filepath.Join(t.TempDir(), "snapshot"))
	if err := runWithConfig(args, api, &bytes.Buffer{}, os.Remove, func(int) string { return filepath.Join(dir, "vdev9.conf") }); err == nil || api.initialized {
		t.Fatalf("error=%v initialized=%t", err, api.initialized)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("release created config directory: %v", err)
	}
}
