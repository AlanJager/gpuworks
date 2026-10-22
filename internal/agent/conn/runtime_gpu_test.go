package conn

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

type runtimeGPUHandlers struct {
	inboundHandlers
	plugin  string
	blocked string
}

func (h *runtimeGPUHandlers) RuntimeGPU(context.Context) (string, string) {
	return h.plugin, h.blocked
}

// Both runtime-plugin facts travel on one status report, and the reason a
// container cannot be given this node's cards is absent from the wire when
// there is none — an older server reads the report unchanged.
func TestNodeStatusWireCarriesThePluginAndWhyItCannotBeUsed(t *testing.T) {
	h := &runtimeGPUHandlers{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, h, &gpu.FixedProber{VendorName: "nvidia"}, slog.Default())
	for _, tc := range []struct {
		plugin, blocked     string
		wantPlugin, wantWhy string
	}{
		{"NVIDIA Container Toolkit 1.17.3", "", `"NVIDIA Container Toolkit 1.17.3"`, ""},
		{"NVIDIA Container Toolkit not installed", accelerator.RuntimeGPUBlockedPluginMissing,
			`"NVIDIA Container Toolkit not installed"`, `"` + accelerator.RuntimeGPUBlockedPluginMissing + `"`},
		{"NVIDIA Container Toolkit 1.17.3", accelerator.RuntimeGPUBlockedRuntimeUnconfigured,
			`"NVIDIA Container Toolkit 1.17.3"`, `"` + accelerator.RuntimeGPUBlockedRuntimeUnconfigured + `"`},
	} {
		h.plugin, h.blocked = tc.plugin, tc.blocked
		ctrl := &nopRWC{}
		c.ctrl = ctrl
		c.reportStatus(context.Background())
		var env proto.Envelope
		if err := json.NewDecoder(strings.NewReader(string(ctrl.BytesCopy()))).Decode(&env); err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Sys map[string]json.RawMessage `json:"sys"`
		}
		if err := json.Unmarshal(env.D, &wire); err != nil {
			t.Fatal(err)
		}
		if got := string(wire.Sys["runtime_plugin"]); got != tc.wantPlugin {
			t.Fatalf("runtime_plugin=%q, want %q in %s", got, tc.wantPlugin, env.D)
		}
		if got := string(wire.Sys["runtime_gpu_blocked"]); got != tc.wantWhy {
			t.Fatalf("runtime_gpu_blocked=%q, want %q in %s", got, tc.wantWhy, env.D)
		}
	}
}

type runtimeFallbackHandlers struct {
	inboundHandlers
	from, reason string
}

func (h *runtimeFallbackHandlers) RuntimeFallback() (string, string) { return h.from, h.reason }

// The runtime a node passed over travels with the reason, and neither key
// is on the wire when nothing was passed over, so an older server reads
// the report unchanged.
func TestNodeStatusWireCarriesTheRuntimePassedOver(t *testing.T) {
	h := &runtimeFallbackHandlers{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, h, &gpu.FixedProber{VendorName: "nvidia"}, slog.Default())
	for _, tc := range []struct {
		from, reason         string
		wantFrom, wantReason string
	}{
		{"", "", "", ""},
		{"docker", accelerator.RuntimeGPUBlockedPluginMissing, `"docker"`, `"gpu_plugin_missing"`},
		{"docker", accelerator.RuntimeGPUBlockedRuntimeUnconfigured, `"docker"`, `"gpu_runtime_unconfigured"`},
		{"docker", accelerator.RuntimeGPUReady, `"docker"`, `"gpu_ready"`},
	} {
		h.from, h.reason = tc.from, tc.reason
		ctrl := &nopRWC{}
		c.ctrl = ctrl
		c.reportStatus(context.Background())
		var env proto.Envelope
		if err := json.NewDecoder(strings.NewReader(string(ctrl.BytesCopy()))).Decode(&env); err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Sys map[string]json.RawMessage `json:"sys"`
		}
		if err := json.Unmarshal(env.D, &wire); err != nil {
			t.Fatal(err)
		}
		if got := string(wire.Sys["runtime_fallback_from"]); got != tc.wantFrom {
			t.Fatalf("runtime_fallback_from=%q, want %q in %s", got, tc.wantFrom, env.D)
		}
		if got := string(wire.Sys["runtime_fallback_reason"]); got != tc.wantReason {
			t.Fatalf("runtime_fallback_reason=%q, want %q in %s", got, tc.wantReason, env.D)
		}
	}
}

// probeHandlers reports a card list and a vendor CLI that failed.
type probeProber struct {
	gpu.FixedProber
	gpus []proto.GPUInfo
	err  error
}

func (p *probeProber) Probe(context.Context) ([]proto.GPUInfo, error) { return p.gpus, p.err }

// A vendor CLI that fails on a node owning none of its cards is the
// node's steady state, not a failed reading: every card on the bus is
// somebody else's, the PCI read that found them answered, and nothing
// shown about them came from the CLI. Reporting it as a probe failure put
// a permanent "the readings are out of date" warning on a Hygon host
// whose only card is passed through.
func TestAVendorCLIWithNoCardToDescribeIsNotAProbeFailure(t *testing.T) {
	lent := proto.GPUInfo{Index: -1, Name: "K100_AI", Vendor: "hygon", PCIeBDF: "0000:67:00.0", Ownership: accelerator.CardPassedThrough}
	ours := proto.GPUInfo{Index: 0, Name: "K100_AI", Vendor: "hygon", PCIeBDF: "0000:68:00.0"}
	for _, tc := range []struct {
		why      string
		gpus     []proto.GPUInfo
		err      error
		wantOK   bool
		wantText bool
	}{
		{"every card is somebody else's", []proto.GPUInfo{lent}, errors.New("exit status 1"), true, false},
		{"one card is still ours", []proto.GPUInfo{lent, ours}, errors.New("exit status 1"), false, true},
		{"no cards at all: a host whose PCI could not be read", nil, errors.New("exit status 1"), false, true},
		{"the CLI answered", []proto.GPUInfo{lent, ours}, nil, true, false},
	} {
		h := &runtimeGPUHandlers{}
		c := New(Credentials{NodeUUID: "node-1"}, nil, h, &probeProber{FixedProber: gpu.FixedProber{VendorName: "hygon"}, gpus: tc.gpus, err: tc.err}, slog.New(slog.DiscardHandler))
		ctrl := &nopRWC{}
		c.ctrl = ctrl
		c.reportStatus(context.Background())
		var env proto.Envelope
		if err := json.Unmarshal(ctrl.BytesCopy(), &env); err != nil {
			t.Fatal(err)
		}
		var st proto.NodeStatus
		if err := json.Unmarshal(env.D, &st); err != nil {
			t.Fatal(err)
		}
		if st.GPUProbeOK == nil || *st.GPUProbeOK != tc.wantOK {
			t.Errorf("%s: probe ok = %v, want %v", tc.why, st.GPUProbeOK, tc.wantOK)
		}
		if (st.GPUProbeError != "") != tc.wantText {
			t.Errorf("%s: probe error = %q, want reported=%v", tc.why, st.GPUProbeError, tc.wantText)
		}
		// Whatever the verdict, every card the bus found still travels.
		if len(st.GPUs) != len(tc.gpus) {
			t.Errorf("%s: reported %d cards, want %d", tc.why, len(st.GPUs), len(tc.gpus))
		}
	}
}
