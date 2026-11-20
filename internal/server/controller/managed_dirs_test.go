package controller

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// releasingHub records the managed directory releases the controller asks for.
type releasingHub struct {
	distributedTestHub
	released []uint
}

func (h *releasingHub) ReleaseDeploymentDirs(id uint) int {
	h.released = append(h.released, id)
	return 1
}

// Deleting a deployment whose template declared directories releases them on
// the nodes once its row is gone, and not before; one that declared none
// sends nothing.
func TestDeletingADeploymentReleasesItsManagedDirectories(t *testing.T) {
	for name, test := range map[string]struct {
		snapshot string
		want     bool
	}{
		"declared": {`{"template_id":1,"model_id":"m","engine":"vllm","image":"i","gpus":{},"dirs":[{"name":"state","mount_path":"/state","limit_bytes":1,"retain":"deployment"}]}`, true},
		"none":     {`{"template_id":1,"model_id":"m","engine":"vllm","image":"i","gpus":{}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("dirs-node", store.HashToken("token"), "dirs-node")
			if err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{NodeID: node.ID, State: store.DeployActive, CatalogSelectionSnapshot: test.snapshot})
			if err != nil {
				t.Fatal(err)
			}
			inst, err := st.CreateInstance(&store.Instance{UUID: "dirs-instance", NodeID: node.ID, DeploymentID: dep.ID, State: proto.InstRunning})
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
				t.Fatal(err)
			}
			hub := &releasingHub{distributedTestHub: distributedTestHub{updates: map[string][]proto.StateUpdate{}}}
			c := New(st, hub, nil)
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			if len(hub.released) != 0 {
				t.Fatalf("released %v while the instance was still being torn down", hub.released)
			}
			if err := st.SetInstanceState(inst.UUID, proto.InstStopped, "agent confirmed teardown"); err != nil {
				t.Fatal(err)
			}
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := st.GetDeployment(dep.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("deployment after teardown = %v", err)
			}
			if got := len(hub.released) == 1 && hub.released[0] == dep.ID; got != test.want {
				t.Fatalf("released = %v, want release %v", hub.released, test.want)
			}
		})
	}
}
