package deployment

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSpecManualCommandIsTheDefaultCommand(t *testing.T) {
	spec := &store.InferenceSpecRow{Name: "pd", ManualCommand: "case \"${GPUWORKS_PD_ROLE}\" in *) exec serve ;; esac"}
	for _, c := range []struct {
		name      string
		overrides map[string]any
		applied   bool
		wantCmd   string
		wantMode  string
	}{
		{name: "no overrides", applied: true, wantCmd: spec.ManualCommand, wantMode: "manual"},
		{name: "empty overrides", overrides: map[string]any{"env": map[string]any{"A": "1"}}, applied: true, wantCmd: spec.ManualCommand, wantMode: "manual"},
		{name: "blank command", overrides: map[string]any{"cmd_display": "  "}, applied: true, wantCmd: spec.ManualCommand, wantMode: "manual"},
		{name: "own command", overrides: map[string]any{"command_mode": "manual", "cmd_display": "exec mine"}, wantCmd: "exec mine", wantMode: "manual"},
		// Naming the engine mode is how a deployer goes back to the
		// engine-built command.
		{name: "engine mode chosen", overrides: map[string]any{"command_mode": "engine"}, wantMode: "engine"},
	} {
		req := dto.DeployRequest{ContainerOverrides: c.overrides}
		applied := applySpecManualCommand(&req, spec)
		cmd, _ := req.ContainerOverrides["cmd_display"].(string)
		mode, _ := req.ContainerOverrides["command_mode"].(string)
		if applied != c.applied || (c.wantCmd != "" && cmd != c.wantCmd) || mode != c.wantMode {
			t.Fatalf("%s: applied %v cmd %q mode %q", c.name, applied, cmd, mode)
		}
		if c.name == "empty overrides" && req.ContainerOverrides["env"] == nil {
			t.Fatalf("%s: other overrides lost: %v", c.name, req.ContainerOverrides)
		}
	}
	// A spec without a command leaves the request alone.
	req := dto.DeployRequest{}
	if applySpecManualCommand(&req, &store.InferenceSpecRow{Name: "plain"}) || req.ContainerOverrides != nil {
		t.Fatalf("plain spec changed the request: %v", req.ContainerOverrides)
	}
	if applySpecManualCommand(&req, nil) {
		t.Fatal("no spec applied a command")
	}
}
