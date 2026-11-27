package agentupgrade

import (
	"fmt"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// testGateway is the inference gateway as an upgrade sees it, with the
// semantics of the proxy's: Avoid hands out its own release token, counted per
// holder; ServedElsewhere answers from what the test says serves elsewhere,
// and changes when the test changes it; nothing is ever refused.
type testGateway struct {
	mu       sync.Mutex
	avoided  map[string]int // instance -> holders
	calls    []string       // preference changes in order, "avoid:<dep>:<uuid>" / "release:<dep>:<uuid>"
	inflight func([]string) int
	// elsewhere answers ServedElsewhere; nil reads it from served.
	elsewhere func(deploymentID uint) bool
	served    map[uint]bool
	looks     int
	last      int // what the last in-flight look found
}

func (g *testGateway) ServedElsewhere(deploymentID uint, _ []string) bool {
	g.mu.Lock()
	elsewhere, served := g.elsewhere, g.served[deploymentID]
	g.mu.Unlock()
	if elsewhere != nil {
		return elsewhere(deploymentID)
	}
	return served
}

func (g *testGateway) Avoid(deploymentID uint, instances []string) func() {
	g.mu.Lock()
	if g.avoided == nil {
		g.avoided = map[string]int{}
	}
	for _, uuid := range instances {
		g.avoided[uuid]++
		g.calls = append(g.calls, fmt.Sprintf("avoid:%d:%s", deploymentID, uuid))
	}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			for _, uuid := range instances {
				if g.avoided[uuid]--; g.avoided[uuid] <= 0 {
					delete(g.avoided, uuid)
				}
				g.calls = append(g.calls, fmt.Sprintf("release:%d:%s", deploymentID, uuid))
			}
		})
	}
}

func (g *testGateway) InstancesInFlight(instances []string) int {
	g.mu.Lock()
	look := g.inflight
	g.mu.Unlock()
	n := 0
	if look != nil {
		n = look(instances)
	}
	g.mu.Lock()
	g.last, g.looks = n, g.looks+1
	g.mu.Unlock()
	return n
}

func (g *testGateway) setServed(deploymentID uint, served bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.served == nil {
		g.served = map[uint]bool{}
	}
	g.served[deploymentID] = served
}
func (g *testGateway) preferenceCalls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// avoiding is how many instances some holder still avoids.
func (g *testGateway) avoiding() int   { g.mu.Lock(); defer g.mu.Unlock(); return len(g.avoided) }
func (g *testGateway) lastServed() int { g.mu.Lock(); defer g.mu.Unlock(); return g.last }
func (g *testGateway) looksTaken() int { g.mu.Lock(); defer g.mu.Unlock(); return g.looks }

// servesOn gives a node a running instance the gateway would route to, of the
// single-node deployment numbered dep (the fixture's own numbering, created on
// first use).
func (f *fixture) servesOn(t *testing.T, uuid, instance string, dep uint) {
	t.Helper()
	node, err := f.st.NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	if f.deployments == nil {
		f.deployments = map[uint]uint{}
	}
	id, ok := f.deployments[dep]
	if !ok {
		name := fmt.Sprintf("dep-%d", dep)
		model, err := f.st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/" + name, Revision: "main", Name: name})
		if err != nil {
			t.Fatal(err)
		}
		d, err := f.st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: name, Engine: "vllm", State: "active"})
		if err != nil {
			t.Fatal(err)
		}
		id = d.ID
		f.deployments[dep] = id
	}
	if _, err := f.st.CreateInstance(&store.Instance{UUID: instance, DeploymentID: id, NodeID: node.ID, State: proto.InstRunning, APIPort: 30002}); err != nil {
		t.Fatal(err)
	}
}
