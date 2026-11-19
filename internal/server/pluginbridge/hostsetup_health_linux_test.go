//go:build linux

package pluginbridge

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/extpkg/hostsetup"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// A real signed package whose root-applied prerequisite is missing cannot
// run: its check must fail and its metrics must name the blocked state.
// Intentionally disabling it clears that failure without starting anything.
func TestHostSetupBlockedPackageCheckAndMetrics(t *testing.T) {
	w := newWorld(t, nil)
	w.records = hostsetup.Records{Dir: t.TempDir(), Owner: os.Getuid()}
	b, sp := w.openRunning()
	step := extapi.HostSetupStep{Type: extapi.HostSetupUSBDevice, VendorID: "096e", ProductID: "0201"}
	install(t, b, driverPkg(w, "1.0.0", step), true)

	views := b.List()
	if len(views) != 1 {
		t.Fatalf("installed views = %+v", views)
	}
	v := views[0]
	if v.ID != "acme_driver" || !v.Enabled || v.State != StateNeedsHostSetup ||
		v.Code != messages.ErrorPluginHostSetupRequired || v.Params["id"] != "acme_driver" ||
		v.Params["command"] != "sudo gpuworks plugin host-setup --id acme_driver" ||
		!slices.Equal(v.Actions, []string{ActionEnable, ActionDisable, ActionUninstall, ActionPurge}) {
		t.Fatalf("missing setup did not reach the expected public refusal: %+v", v)
	}
	if started := sp.started(); len(started) != 0 {
		t.Fatalf("blocked package spawned: %+v", started)
	}
	if entries, err := os.ReadDir(w.records.Dir); err != nil || len(entries) != 0 {
		t.Fatalf("records fixture is not empty: %v, %v", entries, err)
	}
	if c := b.Check(); c.ID != CheckID || c.Status != proto.CheckFail ||
		c.Code != messages.CheckPluginsFailing || c.Params["ids"] != "acme_driver" {
		t.Errorf("check of enabled blocked package = %+v", c)
	}

	assertMetrics := func(active string) {
		t.Helper()
		var out bytes.Buffer
		if err := b.WriteMetrics(&out); err != nil {
			t.Fatal(err)
		}
		text := out.String()
		if !strings.Contains(text, "gpuworks_plugin_up{id=\"acme_driver\"} 0\n") {
			t.Errorf("blocked/disabled package was reported up:\n%s", text)
		}
		// Literal public state vocabulary, independent of the producer's
		// enumeration. Require exactly one active state and all others zero.
		states := []string{"backoff", "broken", "crashloop", "degraded", "disabled", "failed", "idle", "installed",
			"needs_host_setup", "revoked", "running", "starting", "stopped", "stopping"}
		prefix := "gpuworks_plugin_state{id=\"acme_driver\",state=\""
		if n := strings.Count(text, prefix); n != len(states) {
			t.Errorf("state samples = %d, want %d for %s", n, len(states), active)
		}
		for _, state := range states {
			value := 0
			if state == active {
				value = 1
			}
			line := fmt.Sprintf("%s%s\"} %d\n", prefix, state, value)
			if strings.Count(text, line) != 1 {
				t.Errorf("metrics must contain exactly %q", strings.TrimSpace(line))
			}
		}
	}
	assertMetrics("needs_host_setup")

	if _, err := b.Disable(context.Background(), "acme_driver", "admin"); err != nil {
		t.Fatal(err)
	}
	views = b.List()
	if len(views) != 1 || views[0].Enabled || views[0].State != StateDisabled {
		t.Fatalf("disabled package = %+v", views)
	}
	if c := b.Check(); c.ID != CheckID || c.Status != proto.CheckPass || c.Code != messages.CheckPluginsOK {
		t.Errorf("check of intentionally disabled package = %+v", c)
	}
	assertMetrics("disabled")
	if started := sp.started(); len(started) != 0 {
		t.Fatalf("disabling the blocked package spawned: %+v", started)
	}
}
