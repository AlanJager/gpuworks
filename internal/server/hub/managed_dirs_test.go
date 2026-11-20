package hub

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func recordingSession(h *Hub, uuid string, caps []string, groups []string) (*Session, *[]proto.Envelope) {
	var sent []proto.Envelope
	done := make(chan struct{})
	close(done)
	s := &Session{hub: h, NodeUUID: uuid, closed: make(chan struct{}), initialPushDone: done, caps: caps, managedDirGroups: groups,
		sendFn: func(kind string, id uint64, data any) error {
			raw, err := json.Marshal(data)
			sent = append(sent, proto.Envelope{T: kind, ID: id, D: raw})
			return err
		}}
	return s, &sent
}

// A deleted deployment's release goes to every agent that can hold managed
// directories, as one small update, and to no other.
func TestReleaseDeploymentDirsReachesEveryAgentThatHoldsManagedDirs(t *testing.T) {
	h := newTestHub(t)
	holder, holderSent := recordingSession(h, "node-a", []string{proto.CapManagedDirs}, nil)
	other, otherSent := recordingSession(h, "node-b", []string{proto.CapManagedDirs}, nil)
	old, oldSent := recordingSession(h, "node-c", nil, nil)
	h.sessions[holder.NodeUUID], h.sessions[other.NodeUUID], h.sessions[old.NodeUUID] = holder, other, old

	if sent := h.ReleaseDeploymentDirs(7); sent != 2 {
		t.Fatalf("sent to %d sessions, want 2", sent)
	}
	for _, sent := range []*[]proto.Envelope{holderSent, otherSent} {
		if len(*sent) != 1 || (*sent)[0].T != proto.TStateUpdate {
			t.Fatalf("sent = %+v", *sent)
		}
		var update proto.StateUpdate
		if err := json.Unmarshal((*sent)[0].D, &update); err != nil || len(update.ReleasedDirDeployments) != 1 || update.ReleasedDirDeployments[0] != 7 {
			t.Fatalf("update = %s (%v)", (*sent)[0].D, err)
		}
	}
	if len(*oldSent) != 0 {
		t.Fatalf("an agent without managed directories got %+v", *oldSent)
	}
	if sent := h.ReleaseDeploymentDirs(0); sent != 0 {
		t.Fatalf("deployment 0 released to %d sessions", sent)
	}
}

// The replay on connect answers the agent's hello: the groups whose deployment
// is gone are named, a live deployment's group is not.
func TestReplayReleasesTheHelloGroupsOfGoneDeployments(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	live, err := h.store.CreateDeployment(&store.Deployment{Name: "live"})
	if err != nil {
		t.Fatal(err)
	}
	liveGroup := proto.ManagedDirGroupKey(live.ID, live.CreatedAt.Unix())
	goneGroup := proto.ManagedDirGroupKey(live.ID+50, live.CreatedAt.Unix())
	s, _ := recordingSession(h, uuid, []string{proto.CapManagedDirs}, []string{liveGroup, goneGroup})
	ds, _, err := s.buildReplay()
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.ReleasedDirGroups) != 1 || ds.ReleasedDirGroups[0] != goneGroup {
		t.Fatalf("released = %v, want only %s", ds.ReleasedDirGroups, goneGroup)
	}
	plain, _ := recordingSession(h, uuid, nil, nil)
	if ds, _, err := plain.buildReplay(); err != nil || ds.ReleasedDirGroups != nil {
		t.Fatalf("agent without groups released = %v %v", ds.ReleasedDirGroups, err)
	}
}
