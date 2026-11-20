package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// peersFileEnv is the variable the server sets (deploy.PeersFileEnv), which
// the agent passes through like any other.
const peersFileEnv = "GPUWORKS_PEERS_FILE"

func peersFixture() *proto.PeersFile {
	return &proto.PeersFile{
		Version: proto.PeersFileVersion, NodeRank: 1, NNodes: 2, MasterAddr: "192.168.177.51", MasterPort: 29500,
		Network: &proto.PeersNetwork{Type: "host_rdma", LinkLayer: "roce"},
		Ranks: []proto.PeerRank{
			{NodeRank: 0, NodeName: "gb10-1", NodeUUID: "n1", Hostname: "gb10-1", NodeAddr: "192.168.177.51", Management: &proto.ManagementAddress{Interface: "eno1", IP: "10.0.0.51"}, Interface: "enP2p1s0f1np1",
				HCAs: []proto.PeerHCA{{Name: "rocep1s0f1:1", Device: "rocep1s0f1", Port: 1, NetDev: "enp1s0f1np1", IP: "192.168.177.51"}, {Name: "roceP2p1s0f1:1", Device: "roceP2p1s0f1", Port: 1, NetDev: "enP2p1s0f1np1", IP: "192.168.178.51"}}},
			{NodeRank: 1, NodeName: "gb10-2", NodeUUID: "n2", Hostname: "gb10-2", NodeAddr: "192.168.177.52", Management: &proto.ManagementAddress{Interface: "eno1", IP: "10.0.0.52"},
				HCAs: []proto.PeerHCA{{Name: "rocep1s0f1:1", Device: "rocep1s0f1", Port: 1, NetDev: "enp1s0f1np1", IP: "192.168.177.52"}}},
		},
	}
}

// recordingDriver keeps the workload spec of every Run, so a test can read
// the mounts the engine gave the container.
type recordingDriver struct {
	*livenessDriver
	mu   sync.Mutex
	runs []runtime.WorkloadSpec
}

func (d *recordingDriver) Run(ctx context.Context, ws runtime.WorkloadSpec) (string, error) {
	d.mu.Lock()
	d.runs = append(d.runs, ws)
	d.mu.Unlock()
	return d.livenessDriver.Run(ctx, ws)
}

func (d *recordingDriver) lastRun(t *testing.T) runtime.WorkloadSpec {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.runs) == 0 {
		t.Fatal("no container was run")
	}
	return d.runs[len(d.runs)-1]
}

// A rank's peers file is the desired state's document, mounted read-only at
// the path GPUWORKS_PEERS_FILE names, and it goes with the instance.
func TestPeersFileIsMountedReadOnlyAndRemovedWithTheInstance(t *testing.T) {
	driver := &recordingDriver{livenessDriver: newLivenessDriver()}
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("rank-1", healthyEngine(t))
	spec.Peers = peersFixture()
	spec.Env = map[string]string{peersFileEnv: proto.PeersFileMountPath}
	serve(t, e, log, spec)

	ws := driver.lastRun(t)
	var mount *runtime.Mount
	for i := range ws.Mounts {
		if ws.Mounts[i].Target == proto.PeersFileMountPath {
			mount = &ws.Mounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly {
		t.Fatalf("peers file mount = %+v in %+v", mount, ws.Mounts)
	}
	if ws.Env[peersFileEnv] != proto.PeersFileMountPath {
		t.Fatalf("env %s = %q", peersFileEnv, ws.Env[peersFileEnv])
	}
	raw, err := os.ReadFile(mount.Source)
	if err != nil {
		t.Fatal(err)
	}
	var got proto.PeersFile
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&got, spec.Peers) {
		t.Fatalf("peers file = %+v, want %+v", got, *spec.Peers)
	}
	info, err := os.Stat(mount.Source)
	if err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("peers file mode = %v err=%v", info.Mode(), err)
	}

	e.ApplyUpdate(context.Background(), nil, []string{spec.UUID})
	waitGone(t, e, spec.UUID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Dir(mount.Source)); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peers directory %s outlived its instance", filepath.Dir(mount.Source))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A launch without peers gets no file and no mount.
func TestNoPeersFileWithoutPeers(t *testing.T) {
	e := &Engine{stateDir: t.TempDir()}
	ws := runtime.WorkloadSpec{}
	if err := e.applyPeersFile(&ws, proto.InstanceDesiredState{UUID: "single"}); err != nil {
		t.Fatal(err)
	}
	if len(ws.Mounts) != 0 {
		t.Fatalf("mounts = %+v", ws.Mounts)
	}
	if _, err := os.Stat(e.peersDir("single")); !os.IsNotExist(err) {
		t.Fatalf("peers directory created for a single rank: %v", err)
	}
}

// The file follows the desired state on every start: a changed document
// replaces the old one rather than being kept.
func TestPeersFileIsRewrittenWhenItChanges(t *testing.T) {
	e := &Engine{stateDir: t.TempDir()}
	spec := proto.InstanceDesiredState{UUID: "rank-0", ResolvedLaunch: proto.ResolvedLaunch{Peers: peersFixture()}}
	first := runtime.WorkloadSpec{}
	if err := e.applyPeersFile(&first, spec); err != nil {
		t.Fatal(err)
	}
	spec.Peers.MasterPort = 29600
	second := runtime.WorkloadSpec{}
	if err := e.applyPeersFile(&second, spec); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(second.Mounts[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	var got proto.PeersFile
	if err := json.Unmarshal(raw, &got); err != nil || got.MasterPort != 29600 {
		t.Fatalf("rewritten peers file master_port = %d err=%v", got.MasterPort, err)
	}
}

// A peers file that cannot be written fails the launch with its own code
// before any container is created.
func TestPeersFileFailureFailsTheLaunch(t *testing.T) {
	driver := &recordingDriver{livenessDriver: newLivenessDriver()}
	e, log := newLivenessEngine(t, driver)
	// A plain file where the peers purpose directory belongs makes the
	// preparation fail the way a broken state directory would.
	manager := e.runtimeMountManager()
	if err := os.MkdirAll(manager.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manager.purposeRoot(peersMountPurpose), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := runningSpec("rank-broken", healthyEngine(t))
	spec.Peers = peersFixture()
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	failed := log.waitFor(t, proto.InstFailed, 1)
	if got := failed[0].RuntimeFailure; got == nil || got.Code != proto.ErrorPeersFilePrepareFailed {
		t.Fatalf("failure = %+v", got)
	}
	driver.mu.Lock()
	runs := len(driver.runs)
	driver.mu.Unlock()
	if runs != 0 {
		t.Fatalf("a container was run %d times without its peers file", runs)
	}
}
