package controller

import (
	"reflect"
	"slices"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A template that declares api.metrics.port: separate has a second port
// reserved with the API port; the launch states it as ${GPUWORKS_METRICS_PORT},
// the desired state carries it (fixed for the instance's life) and Ports holds
// both so no other instance on the node is given either.
func TestSeparateMetricsPortIsReservedAndReachesTheDesiredState(t *testing.T) {
	spec := scheduleFlatSpecFixture(t, map[string]any{
		"extra_args": []string{"--max-model-len", "512", "--metrics-listen", "${GPUWORKS_METRICS_PORT}"},
		"api":        map[string]any{"metrics": map[string]any{"port": "separate"}},
	})
	if spec.APIPort < 30000 || spec.MetricsPort != spec.APIPort+1 {
		t.Fatalf("api port %d, metrics port %d, want two consecutive engine ports", spec.APIPort, spec.MetricsPort)
	}
	if !reflect.DeepEqual(spec.Ports, []int{spec.APIPort, spec.MetricsPort}) {
		t.Fatalf("ports=%v, want the API port then the metrics port", spec.Ports)
	}
	if got := instanceFlagValues(spec.Argv, "--metrics-listen"); !reflect.DeepEqual(got, []string{strconv.Itoa(spec.MetricsPort)}) {
		t.Fatalf("--metrics-listen=%v argv=%v, want the reserved metrics port", got, spec.Argv)
	}
}

// The default is the API port: no second port is reserved and the desired
// state states none.
func TestMetricsOnTheAPIPortReservesOnePort(t *testing.T) {
	for name, snapshot := range map[string]map[string]any{
		"no api.metrics":   {"extra_args": []string{"--max-model-len", "512"}},
		"port api":         {"extra_args": []string{"--max-model-len", "512"}, "api": map[string]any{"metrics": map[string]any{"port": "api"}}},
		"endpoint changed": {"extra_args": []string{"--max-model-len", "512"}, "api": map[string]any{"metrics": map[string]any{"endpoint": "/m"}}},
	} {
		t.Run(name, func(t *testing.T) {
			spec := scheduleFlatSpecFixture(t, snapshot)
			if spec.MetricsPort != 0 || !slices.Equal(spec.Ports, []int{spec.APIPort}) {
				t.Fatalf("metrics port %d ports %v api port %d", spec.MetricsPort, spec.Ports, spec.APIPort)
			}
		})
	}
}

// A node's next allocation steers clear of a reserved metrics port: an
// instance that still holds it counts as using it.
func TestAMetricsPortIsHeldLikeAnyOtherReservedPort(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("metrics-port-node", store.HashToken("token"), "metrics-port-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{UUID: "held", NodeID: node.ID, State: proto.InstRunning, Ports: jsonInts([]int{30000, 30001}), APIPort: 30000}).Error; err != nil {
		t.Fatal(err)
	}
	c := New(st, &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}, nil)
	ports, rejected := c.pickPorts(node, 1, 1)
	if rejected != nil || !slices.Equal(ports, []int{30002, 30003}) {
		t.Fatalf("ports=%v rejected=%v, want the next two free ports", ports, rejected)
	}
}

// A PD unit reserves the metrics port with the leader's other ports: the API
// port, the metrics port, the side channel block, then the port the unit's
// ranks meet on. A headless follower is reserved none and states zero, as it
// does for its API port.
func TestPDLeaderReservesTheSeparateMetricsPort(t *testing.T) {
	f := newPDFixture(t, "pd_metrics_port", prefillOneDecodeAcrossNodes)
	snapshot := `{"extra_args":["--metrics-listen","${GPUWORKS_METRICS_PORT}"],"api":{"metrics":{"port":"separate"}}}`
	if err := f.st.DB().Model(&store.Deployment{}).Where("id = ?", f.dep.ID).Update("template_snapshot", snapshot).Error; err != nil {
		t.Fatal(err)
	}
	insts := f.reconcile(t)
	prefill, leader, follower := pdByRole(insts)
	if prefill == nil || leader == nil || follower == nil {
		t.Fatalf("instances = %+v", insts)
	}
	for name, inst := range map[string]*store.Instance{"prefill": prefill, "leader": leader} {
		spec := desiredOf(t, *inst)
		held := parseInts(inst.Ports)
		if spec.MetricsPort != spec.APIPort+1 || len(held) < 3 || held[0] != spec.APIPort || held[1] != spec.MetricsPort {
			t.Fatalf("%s: api %d metrics %d ports %v", name, spec.APIPort, spec.MetricsPort, held)
		}
		if got := argAfter(spec.Argv, "--metrics-listen"); got != strconv.Itoa(spec.MetricsPort) {
			t.Fatalf("%s --metrics-listen=%q argv=%v", name, got, spec.Argv)
		}
	}
	record := planning.ReadScheduleRecord(leader.ScheduleExpl).PDRecord()
	if held := parseInts(leader.Ports); !slices.Contains(held, record.Port) || !slices.Contains(held, record.MasterPort) {
		t.Fatalf("leader ports %v miss the side channel %d or the collective port %d", held, record.Port, record.MasterPort)
	}
	spec := desiredOf(t, *follower)
	if spec.MetricsPort != 0 || argAfter(spec.Argv, "--metrics-listen") != "0" {
		t.Fatalf("follower metrics port %d argv=%v", spec.MetricsPort, spec.Argv)
	}
}
