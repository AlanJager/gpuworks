package deployment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nettest"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPDCardNetworkUsesMeasuredPortsNotCardNumbers(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		mutate func(*pdRequest, *netplan.Group, *store.NetworkTestRun, *[]nettest.Pair)
		want   string
	}{
		{name: "different card numbers can reach"},
		{name: "same card number is not proof", mutate: func(p *pdRequest, _ *netplan.Group, _ *store.NetworkTestRun, pairs *[]nettest.Pair) {
			p.pd.Roles[pdmode.RoleDecode] = planning.PDRole{Units: []planning.PDUnit{{NodeUUID: "b", GPUIndexes: []int{0}}}}
			for i := range *pairs {
				(*pairs)[i].Rails = append((*pairs)[i].Rails, nettest.RailResult{Rail: "npu0", TargetRail: "npu0", SourceIP: "10.1.0.1", TargetIP: "10.2.0.1", Code: nettest.CodeUnreachable})
			}
		}, want: messages.WarningPDCardNetworkUnreachable},
		{name: "missing selected pair is unknown", mutate: func(_ *pdRequest, _ *netplan.Group, _ *store.NetworkTestRun, pairs *[]nettest.Pair) {
			*pairs = nil
		}, want: messages.WarningPDCardNetworkUntested},
		{name: "one round trip stored in reverse orientation", mutate: func(p *pdRequest, _ *netplan.Group, _ *store.NetworkTestRun, _ *[]nettest.Pair) {
			p.pd.Roles[pdmode.RolePrefill] = planning.PDRole{Units: []planning.PDUnit{{NodeUUID: "b", GPUIndexes: []int{1}}}}
			p.pd.Roles[pdmode.RoleDecode] = planning.PDRole{Units: []planning.PDUnit{{NodeUUID: "a", GPUIndexes: []int{0}}}}
		}},
		{name: "tool failure is unknown", mutate: func(_ *pdRequest, _ *netplan.Group, _ *store.NetworkTestRun, pairs *[]nettest.Pair) {
			(*pairs)[0].Rails[0].Code = nettest.CodeToolFailed
		}, want: messages.WarningPDCardNetworkUntested},
		{name: "changed IP invalidates proof", mutate: func(_ *pdRequest, _ *netplan.Group, _ *store.NetworkTestRun, pairs *[]nettest.Pair) {
			(*pairs)[0].Rails[0].TargetIP = "10.9.0.1"
		}, want: messages.WarningPDCardNetworkUntested},
		{name: "day old evidence is unknown", mutate: func(_ *pdRequest, _ *netplan.Group, run *store.NetworkTestRun, _ *[]nettest.Pair) {
			run.StartedAt = now.Add(-25 * time.Hour)
		}, want: messages.WarningPDCardNetworkUntested},
		{name: "running test is not proof", mutate: func(_ *pdRequest, _ *netplan.Group, run *store.NetworkTestRun, _ *[]nettest.Pair) {
			run.Status = store.NetworkTestRunning
		}, want: messages.WarningPDCardNetworkUntested},
		{name: "host test cannot prove accelerator fabric", mutate: func(_ *pdRequest, _ *netplan.Group, run *store.NetworkTestRun, _ *[]nettest.Pair) {
			run.Kind = nettest.ModeTCP
		}, want: messages.WarningPDCardNetworkUntested},
		{name: "automatic cards need explicit test", mutate: func(p *pdRequest, _ *netplan.Group, _ *store.NetworkTestRun, _ *[]nettest.Pair) {
			p.pd.Roles[pdmode.RoleDecode] = planning.PDRole{Units: []planning.PDUnit{{NodeUUID: "b"}}}
		}, want: messages.WarningPDCardNetworkUntested},
		{name: "rail excluded from network", mutate: func(_ *pdRequest, group *netplan.Group, _ *store.NetworkTestRun, _ *[]nettest.Pair) {
			group.Ranks[2] = netplan.Resolved{HCAs: []string{"npu0"}}
		}, want: messages.WarningPDCardNetworkUntested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, group, pairs := pdCardTestFixture(t)
			run := &store.NetworkTestRun{Kind: nettest.ModeCardMatrix, Status: store.NetworkTestDone, StartedAt: now.Add(-time.Minute), FinishedAt: &now}
			if tc.mutate != nil {
				tc.mutate(p, &group, run, &pairs)
			}
			encoded, err := json.Marshal(pairs)
			if err != nil {
				t.Fatal(err)
			}
			run.Pairs = string(encoded)
			warnings := pdCardNetworkWarnings(p, group, run, now)
			if tc.want == "" {
				if len(warnings) != 0 {
					t.Fatalf("reachable ports warned: %+v", warnings)
				}
				return
			}
			if len(warnings) != 1 || warnings[0].Code != tc.want {
				t.Fatalf("warnings = %+v, want %s", warnings, tc.want)
			}
			if tc.want == messages.WarningPDCardNetworkUnreachable && !strings.Contains(warnings[0].Params["available"].(string), "b/npu1") {
				t.Fatalf("missing reachable alternative: %+v", warnings)
			}
		})
	}
	p, group, _ := pdCardTestFixture(t)
	if warnings := pdCardNetworkWarnings(p, group, nil, now); len(warnings) != 1 || warnings[0].Code != messages.WarningPDCardNetworkUntested {
		t.Fatalf("no evidence = %+v", warnings)
	}
}

func pdCardTestFixture(t *testing.T) (*pdRequest, netplan.Group, []nettest.Pair) {
	t.Helper()
	var nodes []*store.Node
	group := netplan.Group{Network: netplan.Network{ID: 1, Name: "fabric", Type: netplan.TypeAcceleratorRDMA}, Ranks: map[uint]netplan.Resolved{}}
	for i, uuid := range []string{"a", "b"} {
		inv := proto.NetInventory{AcceleratorPorts: []proto.AcceleratorPort{{Name: "npu0", Index: 0, IP: []string{"10.1.0.1", "10.2.0.1"}[i], State: "UP", Netmask: "255.0.0.0"}, {Name: "npu1", Index: 1, IP: []string{"10.1.0.2", "10.2.0.2"}[i], State: "UP", Netmask: "255.0.0.0"}}}
		data, err := json.Marshal(inv)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, &store.Node{ID: uint(i + 1), UUID: uuid, Name: uuid, GPUVendor: "ascend", NetworkInventory: string(data)})
		group.Ranks[uint(i+1)] = netplan.Resolved{HCAs: []string{"npu0", "npu1"}}
	}
	p := &pdRequest{nodes: nodes, pd: &planning.PDPlacement{Roles: map[string]planning.PDRole{pdmode.RolePrefill: {Units: []planning.PDUnit{{NodeUUID: "a", GPUIndexes: []int{0}}}}, pdmode.RoleDecode: {Units: []planning.PDUnit{{NodeUUID: "b", GPUIndexes: []int{1}}}}}}}
	return p, group, []nettest.Pair{
		{SourceUUID: "a", TargetUUID: "b", Rails: []nettest.RailResult{{Rail: "npu0", TargetRail: "npu1", SourceIP: "10.1.0.1", TargetIP: "10.2.0.2", Reachable: true}}},
	}
}
