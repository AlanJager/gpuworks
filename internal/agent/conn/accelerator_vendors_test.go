package conn

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

type acceleratorVendorHandlers struct {
	inboundHandlers
	vendors []proto.AcceleratorPresence
}

func (h *acceleratorVendorHandlers) AcceleratorVendors() []proto.AcceleratorPresence {
	return h.vendors
}

// The vendors on the host's bus travel on the status report with their
// card counts and models, and the key is absent when there are none, so a
// server that predates the choice reads the report unchanged.
func TestNodeStatusWireCarriesTheVendorsOnTheBus(t *testing.T) {
	h := &acceleratorVendorHandlers{}
	c := New(Credentials{NodeUUID: "node-1"}, nil, h, &gpu.FixedProber{VendorName: "ascend"}, slog.Default())
	for _, tc := range []struct {
		vendors []proto.AcceleratorPresence
		want    string
	}{
		{nil, ""},
		{[]proto.AcceleratorPresence{}, ""},
		{
			[]proto.AcceleratorPresence{{Vendor: "ascend", Cards: 1, Model: "310P3"}, {Vendor: "nvidia", Cards: 1}},
			`[{"vendor":"ascend","cards":1,"model":"310P3"},{"vendor":"nvidia","cards":1}]`,
		},
	} {
		h.vendors = tc.vendors
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
		if got := string(wire.Sys["accelerator_vendors"]); got != tc.want {
			t.Fatalf("accelerator_vendors=%s, want %s in %s", got, tc.want, env.D)
		}
	}
}
