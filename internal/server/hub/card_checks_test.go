package hub

import (
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Every status report's cards reach the card check sink as they arrive,
// including reports the queue would coalesce, and the end of a session tells
// it the node is gone.
func TestEveryReportReachesTheCardCheckSink(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	node, err := h.store.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	type seen struct {
		uuid   string
		nodeID uint
		checks int
	}
	var reports []seen
	var gone []string
	h.SetCardCheckSinks(func(nodeUUID string, nodeID uint, _ time.Time, gpus []proto.GPUInfo) {
		reports = append(reports, seen{nodeUUID, nodeID, len(gpus[0].Checks)})
	}, func(nodeUUID string) { gone = append(gone, nodeUUID) })

	sess := &Session{hub: h, NodeUUID: uuid, nodeID: node.ID, closed: make(chan struct{})}
	h.register(sess)
	for i := 0; i < 3; i++ {
		raw, _ := json.Marshal(proto.NodeStatus{GPUs: []proto.GPUInfo{{UUID: "GPU-1", Checks: []proto.CardCheck{{ID: "runtime", State: "pass"}}}}})
		sess.handleStatusNode(raw)
	}
	if len(reports) != 3 || reports[0] != (seen{uuid, node.ID, 1}) {
		t.Fatalf("reports = %+v, want each of the three", reports)
	}
	h.deregister(sess)
	if len(gone) != 1 || gone[0] != uuid {
		t.Fatalf("gone = %v", gone)
	}
}
