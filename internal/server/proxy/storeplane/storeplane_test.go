package storeplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/proxy/plane"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func openStore(t *testing.T, b *bus.Bus) *store.Store {
	t.Helper()
	st, err := store.Open("file:storeplane_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// The gateway tells changes apart by these strings; they are the store's and
// the bus's own.
func TestPlaneNamesAreTheStoresAndTheBuss(t *testing.T) {
	for _, c := range []struct{ plane, own string }{
		{plane.ReasonProgress, bus.ReasonProgress},
		{plane.ReasonNodeStatus, store.NodeChangeStatus},
		{plane.ReasonNodeScheduling, store.NodeChangeScheduling},
		{plane.TopicInferenceTemplate, store.InferenceTemplateTopic},
	} {
		if c.plane != c.own {
			t.Errorf("plane has %q where the store or bus has %q", c.plane, c.own)
		}
	}
	if !errors.Is(plane.ErrNotFound, store.ErrNotFound) {
		t.Error("plane.ErrNotFound is not the store's")
	}
}

func TestNoStoreIsNoSource(t *testing.T) {
	if src := New(nil); src != nil {
		t.Fatalf("New(nil) = %#v, want no source", src)
	}
	st := openStore(t, bus.New())
	if src, ok := New(st).(*Source); !ok || src.Store != st {
		t.Fatal("New(st) is not a source over st")
	}
}

func seed(t *testing.T, st *store.Store) (store.Deployment, store.Instance, store.Node) {
	t.Helper()
	node := store.Node{UUID: "node-1", Name: "n1", RuntimeDriver: "docker"}
	if err := st.DB().Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	dep := store.Deployment{Name: "d", ServedName: "served", Engine: "vllm", Replicas: 1, NNodes: 1, State: store.DeployActive}
	if err := st.DB().Create(&dep).Error; err != nil {
		t.Fatal(err)
	}
	in := store.Instance{UUID: "inst-1", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001}
	if err := st.DB().Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	return dep, in, node
}

// The snapshot holds the rows the store's routing reads return, and one more
// than the instance and deployment changes announced before they were read.
func TestSnapshotIsTheStoresRoutingRows(t *testing.T) {
	b := bus.New()
	st := openStore(t, b)
	dep, in, node := seed(t, st)
	src := New(st)

	before := b.Generation("instance") + b.Generation("deployment")
	snap, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Generation != before+1 {
		t.Fatalf("generation %d, want %d", snap.Generation, before+1)
	}
	deps, _ := st.RoutingDeployments()
	insts, _ := st.RoutingInstances()
	uuids, drivers, _ := st.RoutingNodes()
	if !reflect.DeepEqual(snap.Deployments, deps) || !reflect.DeepEqual(snap.Instances, insts) ||
		!reflect.DeepEqual(snap.NodeUUID, uuids) || !reflect.DeepEqual(snap.NodeDriver, drivers) {
		t.Fatalf("snapshot differs from the store's routing reads: %+v", snap)
	}
	if len(snap.Deployments) != 1 || snap.Deployments[0].ID != dep.ID || len(snap.Instances) != 1 ||
		snap.Instances[0].UUID != in.UUID || snap.NodeUUID[node.ID] != "node-1" || snap.NodeDriver[node.ID] != "docker" {
		t.Fatalf("snapshot rows: %+v", snap)
	}

	b.PublishChange("instance", in.UUID, "status")
	b.PublishChange("deployment", "d", "updated")
	b.PublishChange("node", "node-1", "status")
	again, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if again.Generation != snap.Generation+2 {
		t.Fatalf("generation moved %d → %d, want +2 (instance and deployment only)", snap.Generation, again.Generation)
	}
	if src.Generation("node") != b.Generation("node") {
		t.Fatal("Generation is not the bus's")
	}
}

// What one member builds its routes from another must be able to receive:
// the snapshot survives encoding and decoding unchanged.
func TestSnapshotSurvivesEncoding(t *testing.T) {
	st := openStore(t, bus.New())
	seed(t, st)
	snap, err := New(st).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var back plane.Snapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	// Structural equality over every field: a field the encoding drops or
	// alters shows here, where encoding the decoded value again would drop
	// it a second time and agree with itself.
	if !reflect.DeepEqual(&back, snap) {
		t.Fatalf("snapshot changed across encoding:\n got %+v\nwant %+v", back, *snap)
	}
	if back.Generation != snap.Generation || len(back.Deployments) != 1 || back.Deployments[0].ServedName != "served" ||
		len(back.Instances) != 1 || back.Instances[0].APIPort != 30001 || back.Instances[0].State != proto.InstRunning ||
		len(back.NodeUUID) != 1 || len(back.NodeDriver) != 1 {
		t.Fatalf("decoded snapshot lost rows: %+v", back)
	}
	for _, key := range []string{`"generation"`, `"deployments"`, `"instances"`, `"node_uuid"`, `"node_driver"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("encoded snapshot has no %s: %s", key, raw)
		}
	}
}

// A lookup of a row that is not there answers as the store does: the
// gateway's not-found, an empty list, or an unresolved key, never a zero row
// that reads as found.
func TestLookupsOfMissingRowsAreNotFound(t *testing.T) {
	st := openStore(t, bus.New())
	src := New(st)
	for name, lookup := range map[string]func() error{
		"GetDeployment":   func() error { _, err := src.GetDeployment(404); return err },
		"InstanceByUUID":  func() error { _, err := src.InstanceByUUID("no-such-instance"); return err },
		"NodeByID":        func() error { _, err := src.NodeByID(404); return err },
		"GetModel":        func() error { _, err := src.GetModel(404); return err },
		"APIKeyScopeByID": func() error { _, err := src.APIKeyScopeByID(404); return err },
	} {
		if err := lookup(); !errors.Is(err, plane.ErrNotFound) {
			t.Errorf("%s of a missing row: %v, want plane.ErrNotFound", name, err)
		}
	}
	if _, ok := src.ResolveAPIKey("gw-no-such-key"); ok {
		t.Error("ResolveAPIKey resolved a key nobody issued")
	}
	if deps, err := src.DeploymentsByServedName("no-such-name"); err != nil || len(deps) != 0 {
		t.Errorf("DeploymentsByServedName of an unknown name: %v, %v", deps, err)
	}
	if insts, err := src.InstancesForDeployment(404); err != nil || len(insts) != 0 {
		t.Errorf("InstancesForDeployment of a missing deployment: %v, %v", insts, err)
	}
}

// A failed read is the snapshot's error, not an empty snapshot.
func TestSnapshotReportsAFailedRead(t *testing.T) {
	st := openStore(t, bus.New())
	if err := st.DB().Exec("DROP TABLE instances").Error; err != nil {
		t.Fatal(err)
	}
	if snap, err := New(st).Snapshot(); err == nil {
		t.Fatalf("snapshot %+v with the instances unreadable", snap)
	}
}

func TestChangesAreTheBussEventsOfTheTopics(t *testing.T) {
	b := bus.New()
	src := New(openStore(t, b))
	changes, stop := src.Changes(8, "instance", "node")
	b.PublishChange("deployment", "d", "updated")
	b.PublishChange("instance", "i1", "status")
	b.PublishChange("node", "n1", store.NodeChangeScheduling)
	for _, want := range []plane.Change{
		{Topic: "instance", Key: "i1", Reason: "status"},
		{Topic: "node", Key: "n1", Reason: "scheduling"},
	} {
		select {
		case got := <-changes:
			if got != want {
				t.Fatalf("change %+v, want %+v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no change for %+v", want)
		}
	}
	stop()
	b.PublishChange("instance", "i2", "status")
	select {
	case got := <-changes:
		t.Fatalf("change %+v after stop", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// A store without a bus (one built by hand; an opened store always has one)
// announces nothing, and says so.
func TestAStoreWithoutABusAnnouncesNothing(t *testing.T) {
	src := New(&store.Store{})
	if src.Announces() {
		t.Fatal("a store without a bus announces changes")
	}
	if src.Generation("instance") != 0 {
		t.Fatal("generation without a bus")
	}
	changes, stop := src.Changes(1, "instance")
	defer stop()
	select {
	case got := <-changes:
		t.Fatalf("change %+v without a bus", got)
	default:
	}
	if !New(openStore(t, bus.New())).Announces() {
		t.Fatal("a store with a bus announces nothing")
	}
}

type fakeHub struct {
	err    error
	online map[string]bool
	opened []string
}

func (h *fakeHub) OpenTunnel(_ context.Context, nodeUUID, dst string) (net.Conn, error) {
	h.opened = append(h.opened, nodeUUID+" "+dst)
	if h.err != nil {
		return nil, h.err
	}
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

func (h *fakeHub) Online(nodeUUID string) bool { return h.online[nodeUUID] }

func TestTunnelsOpenThroughTheHub(t *testing.T) {
	h := &fakeHub{online: map[string]bool{"n1": true}}
	tun := Tunnels(h)
	conn, err := tun.OpenTunnel(context.Background(), "n1", "127.0.0.1:30001")
	if err != nil || conn == nil {
		t.Fatalf("open: %v %v", conn, err)
	}
	_ = conn.Close()
	if len(h.opened) != 1 || h.opened[0] != "n1 127.0.0.1:30001" {
		t.Fatalf("hub saw %v", h.opened)
	}
	if !tun.Online("n1") || tun.Online("n2") {
		t.Fatal("Online is not the hub's")
	}
}

// A detached data lane reads as the gateway's error and as the hub's, at open
// and mid-stream; every failure passes through as it is.
func TestTunnelsNameADetachedDataLane(t *testing.T) {
	lane := fmt.Errorf("%w: node n1 did not reattach within 5s", hub.ErrDataLaneUnavailable)
	_, err := Tunnels(&fakeHub{err: lane}).OpenTunnel(context.Background(), "n1", "127.0.0.1:1")
	if !errors.Is(err, plane.ErrDataLaneUnavailable) || !errors.Is(err, hub.ErrDataLaneUnavailable) {
		t.Fatalf("error %v is not both the plane's and the hub's", err)
	}
	if err != lane {
		t.Fatalf("error %v, want the hub's own value %v", err, lane)
	}
	// One value, so an error the hub raises later, on a read or write of an
	// open stream, is recognised as well without the adapter seeing it.
	if plane.ErrDataLaneUnavailable != hub.ErrDataLaneUnavailable {
		t.Fatal("the gateway's and the hub's data lane errors are two values")
	}
	midStream := fmt.Errorf("%w: node n1 data lane closed under an open stream: %w", hub.ErrDataLaneUnavailable, io.ErrClosedPipe)
	if !errors.Is(midStream, plane.ErrDataLaneUnavailable) {
		t.Fatalf("mid-stream error %v is not the gateway's", midStream)
	}
	other := errors.New("dial refused")
	_, err = Tunnels(&fakeHub{err: other}).OpenTunnel(context.Background(), "n1", "127.0.0.1:1")
	if err != other || errors.Is(err, plane.ErrDataLaneUnavailable) {
		t.Fatalf("error %v, want the hub's own untouched", err)
	}
}
