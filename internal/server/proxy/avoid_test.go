package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// avoidCase is a real gateway in front of an upstream that holds every
// request until the test ends, so what each replica serves can be counted.
type avoidCase struct {
	p     *Proxy
	key   string
	depID uint
	url   string
	ctx   context.Context
}

func newAvoidCase(t *testing.T, replicas int) *avoidCase {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	p, _, key, depID, closeUpstream := newAccountingProxyFor(t, upstream, &store.Deployment{Engine: "vllm", Replicas: replicas})
	front := newFront(t, p)
	ctx, cancel := context.WithCancel(context.Background())
	// Registered after the front server, so it runs first: the held requests
	// end before the servers wait for them to close.
	t.Cleanup(func() {
		cancel()
		once.Do(func() { close(release) })
		closeUpstream()
	})
	return &avoidCase{p: p, key: key, depID: depID, url: front.URL, ctx: ctx}
}

// sibling adds a second replica of the deployment on node (the first
// replica's node when zero) with the given sleep level.
func (c *avoidCase) sibling(t *testing.T, node uint, sleepLevel int) {
	t.Helper()
	st := storeOf(c.p.store)
	first, err := st.InstanceByUUID("omni-instance")
	if err != nil {
		t.Fatal(err)
	}
	if node == 0 {
		node = first.NodeID
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "omni-2", DeploymentID: c.depID, NodeID: node, State: proto.InstRunning, APIPort: 30001, Ports: "[30001]", SleepLevel: sleepLevel}); err != nil {
		t.Fatal(err)
	}
}

// send starts a request and waits until the gateway has leased it to want.
func (c *avoidCase) send(t *testing.T, want string) {
	t.Helper()
	before := c.p.InstancesInFlight([]string{want})
	go func() { _ = postChatNoCleanup(c.ctx, c.url, c.key) }()
	deadline := time.Now().Add(10 * time.Second)
	for c.p.InstancesInFlight([]string{want}) == before {
		if time.Now().After(deadline) {
			held, _ := c.p.HeldRequests(c.depID)
			t.Fatalf("no request reached %s: in flight %d/%d, held %d", want, c.p.InstancesInFlight([]string{"omni-instance"}), c.p.InstancesInFlight([]string{"omni-2"}), held)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// An avoided replica with an awake sibling: new requests go to the sibling,
// from the store's rows and from the route snapshot alike, and come back to
// it once the avoid is released.
func TestAnAvoidedReplicaLeavesItsSiblingServing(t *testing.T) {
	c := newAvoidCase(t, 2)
	c.sibling(t, 0, 0)
	release := c.p.Avoid(c.depID, []string{"omni-instance"})
	c.send(t, "omni-2") // from the rows
	c.p.routes.rebuild()
	if !c.p.ServedElsewhere(c.depID, []string{"omni-instance"}) {
		t.Fatal("ServedElsewhere = false with an awake sibling on an online node")
	}
	c.send(t, "omni-2") // from the snapshot
	c.send(t, "omni-2")
	if got := c.p.InstancesInFlight([]string{"omni-instance"}); got != 0 {
		t.Fatalf("the avoided replica took %d new requests while its sibling could", got)
	}
	release()
	release() // a second release changes nothing
	if c.p.avoid.of(c.depID) != nil {
		t.Fatalf("avoided after release = %v, want none", c.p.avoid.of(c.depID))
	}
}

// The avoided replica is the deployment's only one: it keeps taking requests.
// The upgrade's restart may cut them, but the gateway never answers the model
// as asleep, nor holds the request, for the time the upgrade avoids it.
func TestAnAvoidedOnlyReplicaKeepsServing(t *testing.T) {
	c := newAvoidCase(t, 1)
	c.p.SetWakeDemand(func(uint) { t.Error("an avoided awake model was asked to wake") })
	defer c.p.Avoid(c.depID, []string{"omni-instance"})()
	c.send(t, "omni-instance")
	c.p.routes.rebuild()
	c.send(t, "omni-instance")
	if held, _ := c.p.HeldRequests(c.depID); held != 0 {
		t.Fatalf("%d requests held for an avoided, awake replica", held)
	}
	if c.p.ServedElsewhere(c.depID, []string{"omni-instance"}) {
		t.Fatal("ServedElsewhere = true for a deployment with no other replica")
	}
}

// The sibling is asleep: it cannot take the request, so the avoided replica
// does, and the gateway says the deployment is not served elsewhere.
func TestAnAvoidedReplicaServesWhenItsSiblingSleeps(t *testing.T) {
	c := newAvoidCase(t, 2)
	c.sibling(t, 0, 1)
	defer c.p.Avoid(c.depID, []string{"omni-instance"})()
	c.send(t, "omni-instance")
	c.p.routes.rebuild()
	c.send(t, "omni-instance")
	if c.p.ServedElsewhere(c.depID, []string{"omni-instance"}) {
		t.Fatal("ServedElsewhere = true with the only sibling asleep")
	}
}

// offlineHub is the test hub with some nodes offline.
type offlineHub struct {
	dialTestHub
	offline map[string]bool
}

func (h offlineHub) Online(uuid string) bool { return !h.offline[uuid] }

// The sibling's node is offline (its instance still reads running): the
// avoided replica takes the request.
func TestAnAvoidedReplicaServesWhenItsSiblingsNodeIsOffline(t *testing.T) {
	c := newAvoidCase(t, 2)
	st := storeOf(c.p.store)
	other, err := st.RegisterNode("node-other", store.HashToken("other"), "node-other")
	if err != nil {
		t.Fatal(err)
	}
	c.sibling(t, other.ID, 0)
	c.p.hub = offlineHub{dialTestHub: c.p.hub.(dialTestHub), offline: map[string]bool{other.UUID: true}}
	defer c.p.Avoid(c.depID, []string{"omni-instance"})()
	c.send(t, "omni-instance")
	c.p.routes.rebuild()
	c.send(t, "omni-instance")
	if c.p.ServedElsewhere(c.depID, []string{"omni-instance"}) {
		t.Fatal("ServedElsewhere = true with the only sibling on an offline node")
	}
}

// Two holders avoid the same instance: it is preferred against until both
// have released it.
func TestAvoidHoldsAreCountedPerHolder(t *testing.T) {
	var p Proxy
	first := p.Avoid(7, []string{"inst"})
	second := p.Avoid(7, []string{"inst"})
	first()
	first()
	if !p.avoid.of(7)["inst"] {
		t.Fatal("one holder's release (twice) dropped the other's avoid")
	}
	second()
	if p.avoid.of(7) != nil {
		t.Fatalf("avoided after both released = %v, want none", p.avoid.of(7))
	}
}

// refusingHub is the test hub refusing tunnels to one port, as a detached
// data lane does.
type refusingHub struct {
	dialTestHub
	port string
}

func (h refusingHub) OpenTunnel(ctx context.Context, node, dst string) (net.Conn, error) {
	if strings.HasSuffix(dst, ":"+h.port) {
		return nil, errors.New("tunnel refused")
	}
	return h.dialTestHub.OpenTunnel(ctx, node, dst)
}

// The preferred sibling cannot be reached: the request is retried on the
// avoided replica rather than failed, since an avoid only orders the
// replicas a request tries.
func TestAnAvoidedReplicaTakesTheRetryWhenItsSiblingFails(t *testing.T) {
	c := newAvoidCase(t, 2)
	c.sibling(t, 0, 0)
	c.p.hub = refusingHub{dialTestHub: c.p.hub.(dialTestHub), port: "30001"}
	defer c.p.Avoid(c.depID, []string{"omni-instance"})()
	c.send(t, "omni-instance")
	c.p.routes.rebuild()
	c.send(t, "omni-instance")
}

// avoidingProxy is a gateway with nothing but a balancer and a hub, enough
// for the choice among candidates.
func avoidingProxy(now time.Time) *Proxy {
	b := newBalancer(nil)
	b.now = func() time.Time { return now }
	return &Proxy{balancer: b, hub: dialTestHub{}}
}

// The only sibling is cooling down after a failed connection: the balancer
// would send it requests only as the last thing left, and the avoided replica
// is left, so the request goes to the avoided replica rather than first to the
// replica known to be failing.
func TestAnAvoidedReplicaServesWhenItsSiblingCoolsDown(t *testing.T) {
	now := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	p := avoidingProxy(now)
	avoided := candidate{instanceUUID: "avoided", nodeUUID: "n1"}
	sibling := candidate{instanceUUID: "sibling", nodeUUID: "n2"}
	p.balancer.state("sibling").cooldownUntil = now.Add(30 * time.Second)
	// Candidates read for this request (the snapshot path also checks rows,
	// which this gateway has none of, and is covered with a real one above).
	for _, policy := range []string{deploy.LeastRequest, deploy.Failover, deploy.PrefixAffinity} {
		l, busy := p.pickAvoiding([]candidate{avoided, sibling}, map[string]bool{"avoided": true}, policy, "conversation", true, admission{})
		if l == nil || busy || l.c.instanceUUID != "avoided" {
			t.Fatalf("%s: picked %v busy %v, want the avoided replica over a cooling sibling", policy, l, busy)
		}
		l.release(outcomeCanceled)
	}
	// Once the sibling is out of its cool-down it is preferred again.
	p.balancer.state("sibling").cooldownUntil = time.Time{}
	l, _ := p.pickAvoiding([]candidate{avoided, sibling}, map[string]bool{"avoided": true}, deploy.LeastRequest, "", true, admission{})
	if l == nil || l.c.instanceUUID != "sibling" {
		t.Fatalf("picked %v, want the recovered sibling", l)
	}
}

// The sibling has no free run slot for a request that asked to be refused when
// busy: the avoided replica, which has one, takes it instead of the client
// being told the model is busy.
func TestAnAvoidedReplicaServesWhenItsSiblingIsFull(t *testing.T) {
	p := avoidingProxy(time.Now())
	avoided := candidate{instanceUUID: "avoided", nodeUUID: "n1", slots: 4}
	sibling := candidate{instanceUUID: "sibling", nodeUUID: "n2", slots: 1}
	p.balancer.take(sibling, admission{})
	l, busy := p.pickAvoiding([]candidate{avoided, sibling}, map[string]bool{"avoided": true}, deploy.LeastRequest, "", true, declared(0))
	if l == nil || busy || l.c.instanceUUID != "avoided" {
		t.Fatalf("picked %v busy %v, want the avoided replica with room", l, busy)
	}
}

// A retry prefers the replicas nothing avoids, and reaches the avoided one
// only when none of them is left.
func TestARetryTriesTheAvoidedReplicaLast(t *testing.T) {
	b := newBalancer(nil)
	tr := &retryTransport{balancer: b, hub: dialTestHub{}}
	a := &routeAttempt{
		candidates: []candidate{{instanceUUID: "avoided", nodeUUID: "n1"}, {instanceUUID: "b", nodeUUID: "n2"}, {instanceUUID: "c", nodeUUID: "n3"}},
		tried:      map[string]bool{"b": true},
		policy:     deploy.LeastRequest,
		avoided:    map[string]bool{"avoided": true},
	}
	// c carries more load than the avoided replica, so least-request alone
	// would pick the avoided one.
	b.take(candidate{instanceUUID: "c"}, admission{})
	l := tr.pickNext(a, false)
	if l == nil || l.c.instanceUUID != "c" {
		t.Fatalf("retry picked %v, want c before the avoided replica", l)
	}
	l.release(outcomeCanceled)
	a.tried["c"] = true
	if l = tr.pickNext(a, false); l == nil || l.c.instanceUUID != "avoided" {
		t.Fatalf("retry picked %v, want the avoided replica once nothing else is left", l)
	}
}

// Avoiding nothing reads no lock: the view is one atomic load.
func TestAvoidSetReadsWithoutTheWriterLock(t *testing.T) {
	var p Proxy
	release := p.Avoid(3, []string{"x"})
	p.avoid.mu.Lock()
	got := p.avoid.of(3)["x"]
	other := p.avoid.of(4)
	p.avoid.mu.Unlock()
	if !got || other != nil {
		t.Fatalf("of(3)[x]=%v of(4)=%v while a writer holds the lock", got, other)
	}
	release()
}
