package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitializationFailureStopsBeforeNativeQueries(t *testing.T) {
	initErr := errors.New("rsmi_init(0) failed: status=7")
	api := &fakeAPI{initErr: initErr}
	var out bytes.Buffer
	err := run([]string{"inspect", "--vdev-id", "9"}, api, &out)
	if !errors.Is(err, initErr) || !strings.Contains(err.Error(), "status=7") || !api.initialized || api.shutdown || len(api.calls) != 0 || out.Len() != 0 {
		t.Fatalf("error=%v initialized=%t shutdown=%t calls=%v output=%q", err, api.initialized, api.shutdown, api.calls, out.String())
	}
}

func TestShutdownFailureIsReturnedWithOperationError(t *testing.T) {
	inspectErr := errors.New("device query failed")
	shutdownErr := errors.New("runtime shutdown failed")
	api := &fakeAPI{inspectErr: inspectErr, shutdownErr: shutdownErr}
	var out bytes.Buffer
	err := run([]string{"inspect", "--vdev-id", "9"}, api, &out)
	if !errors.Is(err, inspectErr) || !errors.Is(err, shutdownErr) || !api.shutdown || out.Len() != 0 {
		t.Fatalf("error=%v shutdown=%t output=%q", err, api.shutdown, out.String())
	}
}

func TestShutdownFailureDoesNotReportSuccessfulCommand(t *testing.T) {
	shutdownErr := errors.New("runtime shutdown failed")
	api := &fakeAPI{shutdownErr: shutdownErr}
	var out bytes.Buffer
	if err := run([]string{"inspect", "--vdev-id", "9"}, api, &out); !errors.Is(err, shutdownErr) || !api.shutdown {
		t.Fatalf("error=%v shutdown=%t", err, api.shutdown)
	}
}

func TestReleaseAcceptsConfigRemovedByNativeDestroy(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "vdev9.conf")
	snapshot := filepath.Join(dir, "snapshot.conf")
	for _, path := range []string{host, snapshot} {
		if err := os.WriteFile(path, []byte("owned config\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCurrentOwner(dir, 9, currentOwner{Token: testOwnerToken, Active: true}); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{exists: true, info: expectedInfo}
	api.destroyFn = func(id int) error {
		if id != 9 {
			t.Fatalf("destroyed unowned ID %d", id)
		}
		api.exists = false
		return os.Remove(host)
	}
	args := append(append([]string{}, releaseArgs...), "--snapshot-path", snapshot)
	var out bytes.Buffer
	if err := runWithConfig(args, api, &out, os.Remove, func(int) string { return host }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(api.calls, ",") != "inspect,stop,destroy" || out.String() != "{\"vdev_id\":9}\n" {
		t.Fatalf("calls=%v output=%q", api.calls, out.String())
	}
	if _, err := os.Stat(host); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native config remains: %v", err)
	}
	if data, err := os.ReadFile(snapshot); err != nil || string(data) != "owned config\n" {
		t.Fatalf("owned snapshot changed: %q, %v", data, err)
	}
	owner, err := readCurrentOwner(dir, 9)
	if err != nil || owner.Active || owner.Token != testOwnerToken {
		t.Fatalf("released owner=%+v, %v", owner, err)
	}
}
