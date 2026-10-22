package hub

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
)

func networkTestSession(t *testing.T, h *Hub, caps []string, send func(string, uint64, any) error) *Session {
	t.Helper()
	uuid, _ := newTestNode(t, h.store)
	s := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{}), initialPushDone: make(chan struct{}), caps: caps, sendFn: send}
	close(s.initialPushDone)
	h.mu.Lock()
	h.sessions[uuid] = s
	h.mu.Unlock()
	t.Cleanup(s.markClosed)
	return s
}

func receiveNetworkEnvelope(t *testing.T, sent <-chan proto.Envelope) proto.Envelope {
	t.Helper()
	select {
	case env := <-sent:
		return env
	case <-time.After(2 * time.Second):
		t.Fatal("network update did not arrive")
		return proto.Envelope{}
	}
}

func TestNetworkRequestPreservesDerivedPolicyAndLegacyReplay(t *testing.T) {
	for _, modern := range []bool{true, false} {
		t.Run(map[bool]string{true: "incremental", false: "legacy"}[modern], func(t *testing.T) {
			h := newTestHub(t)
			h.SetEnginePorts(func() proto.PortRange { return proto.PortRange{Start: 8100, End: 8199} })
			setProxies(t, h.store, everyNode("http://global.invalid", "http://secure.invalid"))
			sent := make(chan proto.Envelope, 1)
			var caps []string
			if modern {
				caps = []string{proto.CapNetworkUpdate}
			}
			s := networkTestSession(t, h, caps, func(kind string, id uint64, data any) error {
				raw, err := json.Marshal(data)
				sent <- proto.Envelope{T: kind, D: raw}
				return err
			})
			node, err := h.store.NodeByUUID(s.NodeUUID)
			if err != nil {
				t.Fatal(err)
			}
			setProxies(t, h.store, forNode(node.UUID, "http://node.invalid", "http://secure.invalid"), everyNode("http://global.invalid", "http://secure.invalid"))
			if err := h.store.UpdateNodeStatus(s.NodeUUID, proto.NodeStatus{Network: &proto.NetInventory{Interfaces: []proto.NetInterface{{Name: "eth1", Addrs: []string{"192.0.2.5/24"}}}}}); err != nil {
				t.Fatal(err)
			}
			networkService := network.New(h.store, network.StoreCatalog{Store: h.store})
			h.SetNetworks(networkService)
			serving, err := networkService.CreateNetwork(network.NetworkSpec{Name: "serving", Roles: []string{netplan.RoleServing}, Type: netplan.TypeHostEthernet, CIDR: "192.0.2.0/24"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := networkService.Attach(serving.ID, network.AttachmentSpec{NodeUUID: s.NodeUUID}); err != nil {
				t.Fatal(err)
			}
			h.RequestNetworkSync(s.NodeUUID)
			env := receiveNetworkEnvelope(t, sent)
			var payload struct {
				Network proto.NetworkConfig `json:"network"`
			}
			if err := json.Unmarshal(env.D, &payload); err != nil {
				t.Fatal(err)
			}
			cfg := payload.Network
			if cfg.ServingIP != "192.0.2.5" || cfg.HTTPProxy != "http://node.invalid" || cfg.HTTPSProxy != "http://secure.invalid" || cfg.EnginePorts == nil || *cfg.EnginePorts != (proto.PortRange{Start: 8100, End: 8199}) {
				t.Fatalf("incomplete effective policy: %+v", cfg)
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(env.D, &fields)
			if modern {
				if env.T != proto.TStateUpdate {
					t.Fatalf("modern agent received %s", env.T)
				}
				for _, key := range []string{"instances", "storages", "benchmarks", "model_files", "orphan_cleanup", "runtime_preference", "registry_auth"} {
					if _, ok := fields[key]; ok {
						t.Fatalf("network delta included unrelated %s", key)
					}
				}
			} else if env.T != proto.TStateSync || fields["instances"] == nil || fields["storages"] == nil {
				t.Fatalf("legacy baseline missing: %s %s", env.T, env.D)
			}
		})
	}
}

func TestNetworkRequestCoalescesBlockedNodeWithoutDelayingAnother(t *testing.T) {
	h := newTestHub(t)
	h.pushLimiter = make(chan struct{}, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	sent := make(chan proto.Envelope, 4)
	calls := 0
	stalled := networkTestSession(t, h, []string{proto.CapNetworkUpdate}, func(kind string, id uint64, data any) error {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		raw, err := json.Marshal(data)
		sent <- proto.Envelope{T: kind, D: raw}
		return err
	})
	otherSent := make(chan proto.Envelope, 1)
	other := networkTestSession(t, h, []string{proto.CapNetworkUpdate}, func(kind string, id uint64, data any) error {
		raw, err := json.Marshal(data)
		otherSent <- proto.Envelope{T: kind, D: raw}
		return err
	})
	setProxies(t, h.store, everyNode("http://first.invalid", ""))
	h.RequestNetworkSync(stalled.NodeUUID)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first send never started")
	}
	setProxies(t, h.store, everyNode("http://latest.invalid", ""))
	for range 100 {
		h.RequestNetworkSync(stalled.NodeUUID)
	}
	h.RequestNetworkSync(other.NodeUUID)
	receiveNetworkEnvelope(t, otherSent)
	unblock()
	receiveNetworkEnvelope(t, sent)
	env := receiveNetworkEnvelope(t, sent)
	var data proto.StateUpdate
	if err := json.Unmarshal(env.D, &data); err != nil {
		t.Fatal(err)
	}
	if data.Network == nil || data.Network.HTTPProxy != "http://latest.invalid" {
		t.Fatalf("latest save lost: %s", env.D)
	}
	select {
	case extra := <-sent:
		t.Fatalf("burst was not coalesced: %s", extra.D)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNetworkRequestReadsAfterFullReplayAndStopsWhenClosed(t *testing.T) {
	h := newTestHub(t)
	sent := make(chan proto.Envelope, 4)
	s := networkTestSession(t, h, []string{proto.CapNetworkUpdate}, func(kind string, id uint64, data any) error {
		raw, err := json.Marshal(data)
		sent <- proto.Envelope{T: kind, D: raw}
		return err
	})
	s.fullPushMu.Lock()
	h.RequestNetworkSync(s.NodeUUID)
	setProxies(t, h.store, everyNode("http://after-replay.invalid", ""))
	select {
	case <-sent:
		s.fullPushMu.Unlock()
		t.Fatal("network delta overtook full replay")
	default:
	}
	s.fullPushMu.Unlock()
	env := receiveNetworkEnvelope(t, sent)
	var data proto.StateUpdate
	_ = json.Unmarshal(env.D, &data)
	if data.Network == nil || data.Network.HTTPProxy != "http://after-replay.invalid" {
		t.Fatalf("stale policy after replay: %s", env.D)
	}
	s.markClosed()
	for range 10 {
		h.RequestNetworkSync(s.NodeUUID)
	}
	h.RequestNetworkSync("offline-node")
	select {
	case <-sent:
		t.Fatal("closed session received another policy")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestNetworkRequestClosedDuringInitialReplayNeverSends(t *testing.T) {
	for _, caps := range [][]string{{proto.CapNetworkUpdate}, nil} {
		t.Run(map[bool]string{true: "modern", false: "legacy"}[len(caps) > 0], func(t *testing.T) {
			h := newTestHub(t)
			sent := make(chan proto.Envelope, 1)
			uuid, _ := newTestNode(t, h.store)
			s := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{}), initialPushDone: make(chan struct{}), caps: caps, sendFn: func(kind string, id uint64, data any) error { sent <- proto.Envelope{T: kind}; return nil }}
			h.sessions[uuid] = s
			h.RequestNetworkSync(uuid)
			s.markClosed()
			close(s.initialPushDone)
			select {
			case <-sent:
				t.Fatal("closed initial session received network update")
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}
