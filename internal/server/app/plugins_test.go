package app

import (
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginbridge"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// A wire refusal from pkg/extapi reaches the API as a product error with
// its code and params; product errors and plain errors pass unchanged.
func TestPluginRefusalsBecomeProductErrors(t *testing.T) {
	wire := &extapi.Error{Code: extapi.ErrorStateTooNew, Message: "too new", Params: map[string]any{"marker": "plugin.acme_x.data"}}
	var pe *messages.ProductError
	if err := productErrorOf(wire); !errors.As(err, &pe) || pe.Code() != extapi.ErrorStateTooNew || pe.Params()["marker"] != "plugin.acme_x.data" || !errors.Is(err, wire) {
		t.Fatalf("wire refusal = %v", err)
	}
	product := messages.NewProductError(messages.ErrorPluginBroken, "broken", nil)
	plain := errors.New("disk")
	if productErrorOf(product) != error(product) || productErrorOf(plain) != plain || productErrorOf(nil) != nil {
		t.Fatal("a product error, a plain error or nil was changed")
	}
}

// The DTO carries every field of the view, lists are never null, a reason
// becomes a product message, and no time is sent for an unsupervised
// package.
func TestPluginDTO(t *testing.T) {
	since := time.Date(2026, 11, 17, 9, 0, 0, 0, time.FixedZone("x", 3600))
	got := pluginDTO(pluginbridge.View{ID: "acme_backup", Version: "1.0.0", Publisher: "acme", Trust: "project", Enabled: true, State: "crashloop",
		Code: messages.ErrorPluginCrashloop, Params: map[string]any{"crashes": 5}, Message: "crashed", Since: since, Restarts: 4,
		Parts: []string{"server"}, Isolation: "landlock", Permissions: []string{"http:routes", "push:node_holds", "read:fleet"},
		ApproveHash: "sha256:00", Previous: "0.9.0", OnDemand: true, Pending: "1.1.0",
		Features: []pluginbridge.FeatureView{{Key: "acme.backup", Access: "read_only"}}, Actions: []string{"disable", "uninstall"}})
	utc := since.UTC()
	want := dto.Plugin{ID: "acme_backup", Version: "1.0.0", Publisher: "acme", Trust: "project", Enabled: true, State: "crashloop",
		Message: &dto.ProductMessage{Code: messages.ErrorPluginCrashloop, Params: map[string]any{"crashes": 5}, Message: "crashed"}, Since: &utc, Restarts: 4,
		Parts: []string{"server"}, Isolation: "landlock", Roles: []string{},
		Permissions: []dto.PluginPermission{{Name: "http:routes", Risk: "low"}, {Name: "push:node_holds", Risk: "high"}, {Name: "read:fleet", Risk: "elevated"}},
		ApproveHash: "sha256:00", Previous: "0.9.0", OnDemand: true, Pending: &dto.PluginPending{Version: "1.1.0"},
		Features: []dto.PluginFeature{{Key: "acme.backup", Availability: "read_only"}}, Actions: []string{"disable", "uninstall"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dto = %+v\nwant %+v", got, want)
	}
	bare := pluginDTO(pluginbridge.View{ID: "acme_x", State: "broken"})
	if bare.Message != nil || bare.Since != nil || bare.Pending != nil || bare.Parts == nil || bare.Actions == nil || bare.Permissions == nil || bare.Features == nil {
		t.Fatalf("bare dto = %+v", bare)
	}
}

// Plugins get cgroups only when the server's cgroup is delegated and the
// server moved into its leaf; otherwise they share its cgroup.
func TestPluginCgroupsFallBackToTheServersCgroup(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	detected := func(unit string) (string, string, bool) {
		if unit != "gpuworks.service" {
			t.Fatalf("detect asked for unit %q", unit)
		}
		return "/sys/fs/cgroup", "system.slice/gpuworks.service", true
	}
	var entered []string
	enter := func(root, parent string, _ int) error { entered = append(entered, root+"/"+parent); return nil }
	if root, parent := pluginCgroups(log, detected, enter); root != "/sys/fs/cgroup" || parent != "system.slice/gpuworks.service" ||
		len(entered) != 1 || entered[0] != "/sys/fs/cgroup/system.slice/gpuworks.service" {
		t.Fatalf("delegated: %q %q, entered %v", root, parent, entered)
	}
	refuse := func(string, string, int) error { return errors.New("cgroup.procs: permission denied") }
	if root, parent := pluginCgroups(log, detected, refuse); root != "" || parent != "" {
		t.Fatalf("the server could not enter its leaf yet plugins got cgroups: %q %q", root, parent)
	}
	notDelegated := func(string) (string, string, bool) { return "", "", false }
	if root, parent := pluginCgroups(log, notDelegated, enter); root != "" || parent != "" || len(entered) != 1 {
		t.Fatalf("not delegated: %q %q, entered %v", root, parent, entered)
	}
}
