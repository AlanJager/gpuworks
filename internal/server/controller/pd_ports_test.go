package controller

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A multi-node SGLang prefill serves its bootstrap on one port on every rank,
// so the port is picked among those every node of the unit leaves free: a
// port another instance holds on the second node is skipped on the first too.
func TestPickPortsOnAvoidsEveryNodesPorts(t *testing.T) {
	st, err := store.Open("file:pd_pick_ports_on?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	first, err := st.RegisterNode("first", store.HashToken("t1"), "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.RegisterNode("second", store.HashToken("t2"), "second")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "held-first", NodeID: first.ID, State: proto.InstRunning, Ports: "[40000]"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "held-second", NodeID: second.ID, State: proto.InstRunning, Ports: "[40001,40002]"}); err != nil {
		t.Fatal(err)
	}
	// A stopped instance's ports are free again.
	if _, err := st.CreateInstance(&store.Instance{UUID: "stopped-second", NodeID: second.ID, State: proto.InstStopped, Ports: "[40003]"}); err != nil {
		t.Fatal(err)
	}
	c := New(st, nil, nil)
	c.SetEnginePorts(40000, 40005)
	// Alone, the first node would hand out 40001.
	if got, rejected := c.pickPorts(first, 1); rejected != nil || fmt.Sprint(got) != "[40001]" {
		t.Fatalf("first alone = %v %v", got, rejected)
	}
	got, rejected := c.pickPortsOn([]*store.Node{first, second}, 1, 1)
	if rejected != nil || fmt.Sprint(got) != "[40003 40004]" {
		t.Fatalf("shared blocks = %v %v", got, rejected)
	}
	// 40003-40005 are all both nodes leave free: a fourth block does not fit.
	if got, rejected := c.pickPortsOn([]*store.Node{first, second}, 1, 1, 1, 1); rejected == nil || got != nil {
		t.Fatalf("oversized = %v %v", got, rejected)
	}
}
