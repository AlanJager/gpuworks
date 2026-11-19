package pluginbridge

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg/hostsetup"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func driverPkg(w *world, version string, steps ...extapi.HostSetupStep) []byte {
	return w.pkg("acme_driver", version, func(s *extsign.Spec) {
		s.Manifest.Requires = map[string]int{extapi.CapHostSetup: 1}
		s.Manifest.HostSetup = steps
	})
}

// apply writes package id's applied record as root would, for steps.
func apply(t *testing.T, r hostsetup.Records, id string, steps ...extapi.HostSetupStep) {
	t.Helper()
	d, err := hostsetup.Digest(steps)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := r.Path(id)
	if err := os.WriteFile(path, hostsetup.Content(d), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A package with host setup steps waits in needs_host_setup, naming the
// command, and starts nothing until root has applied exactly its steps;
// then it starts with the device directories and read paths of its
// sandbox. The view offers Enable as the way to check again once the
// command ran. An upgrade that changes the steps waits again, while one
// record the service account could have written never counts.
func TestHostSetupGatesTheStart(t *testing.T) {
	w := newWorld(t, nil)
	w.records = hostsetup.Records{Dir: t.TempDir(), Owner: os.Getuid()}
	b, sp := w.openRunning()
	key := extapi.HostSetupStep{Type: extapi.HostSetupUSBDevice, VendorID: "096e", ProductID: "0201"}
	other := extapi.HostSetupStep{Type: extapi.HostSetupUSBDevice, VendorID: "1050", ProductID: "0407"}

	v := install(t, b, driverPkg(w, "1.0.0", key), true).View
	if v.State != StateNeedsHostSetup || v.Code != messages.ErrorPluginHostSetupRequired ||
		v.Params["id"] != "acme_driver" || v.Params["command"] != "sudo gpuworks plugin host-setup --id acme_driver" ||
		!slices.Equal(v.Actions, []string{ActionEnable, ActionDisable, ActionUninstall, ActionPurge, ActionUpgrade}) {
		t.Fatalf("pending = %+v", v)
	}
	if got := sp.started(); len(got) != 0 {
		t.Fatalf("a package without its host setup was started: %+v", got)
	}

	apply(t, w.records, "acme_driver", other)
	if _, err := b.Enable(context.Background(), "acme_driver", "admin"); err != nil {
		t.Fatal(err)
	}
	if v := b.view("acme_driver"); v.State != StateNeedsHostSetup || len(sp.started()) != 0 {
		t.Fatalf("with a record for other steps = %+v, started %d", v, len(sp.started()))
	}

	apply(t, w.records, "acme_driver", key)
	if _, err := b.Enable(context.Background(), "acme_driver", "admin"); err != nil {
		t.Fatal(err)
	}
	waitView(t, b, "acme_driver", func(v View) bool { return v.State == string(pluginhost.StateBackoff) })
	got := sp.started()
	if len(got) != 1 || !slices.Equal(got[0].DeviceDirs, []string{"/dev"}) || !slices.Contains(got[0].ReadPaths, "/sys/devices") {
		t.Fatalf("started = %+v", got)
	}

	v = install(t, b, driverPkg(w, "2.0.0", key, other), true).View
	if v.State != StateNeedsHostSetup || v.Version != "2.0.0" || len(sp.started()) != 1 {
		t.Fatalf("after an upgrade that changed the steps = %+v, started %d", v, len(sp.started()))
	}

	path, _ := w.records.Path("acme_driver")
	apply(t, w.records, "acme_driver", key, other)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(context.Background(), "acme_driver", "admin"); err != nil {
		t.Fatal(err)
	}
	if v := b.view("acme_driver"); v.State != StateNeedsHostSetup || len(sp.started()) != 1 {
		t.Fatalf("with a world-writable record = %+v", v)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(context.Background(), "acme_driver", "admin"); err != nil {
		t.Fatal(err)
	}
	waitView(t, b, "acme_driver", func(v View) bool { return v.State == string(pluginhost.StateBackoff) && v.Version == "2.0.0" })
	if len(sp.started()) < 2 {
		t.Fatalf("2.0.0 did not start once applied: %d starts", len(sp.started()))
	}
	if _, err := os.Stat(filepath.Join(w.records.Dir, "acme_driver.applied")); err != nil {
		t.Fatal(err)
	}
}

// A package without host setup steps starts with no device directory.
func TestNoHostSetupNoDeviceDirs(t *testing.T) {
	w := newWorld(t, nil)
	w.records = hostsetup.Records{Dir: t.TempDir(), Owner: os.Getuid()}
	b, sp := w.openRunning()
	install(t, b, w.pkg("acme_driver", "1.0.0", nil), true)
	waitView(t, b, "acme_driver", func(v View) bool { return v.State == string(pluginhost.StateBackoff) })
	if got := sp.started(); len(got) == 0 || got[0].DeviceDirs != nil || got[0].ReadPaths != nil {
		t.Fatalf("started = %+v", got)
	}
}
