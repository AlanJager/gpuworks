package store

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

func TestDesiredStateForNodeExcludesTerminalAndPendingRemovalInstances(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("desired-node", HashToken("token"), "desired-host")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeRuntimePreference(node.UUID, proto.RuntimePreferenceDocker); err != nil {
		t.Fatal(err)
	}
	makeSpec := func(uuid string) string {
		raw, err := json.Marshal(proto.InstanceDesiredState{
			UUID: uuid, Image: "gpustack/test:latest", DesiredState: "running", GPUIndexes: []int{0}, APIPort: 8000,
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, tc := range []struct {
		uuid  string
		state string
		want  bool
	}{
		{uuid: "running", state: proto.InstRunning, want: true},
		{uuid: "starting", state: proto.InstStarting, want: true},
		{uuid: "pending", state: proto.InstPending, want: true},
		{uuid: "failed", state: proto.InstFailed, want: false},
		{uuid: "stopped", state: proto.InstStopped, want: false},
		{uuid: "removing", state: "pending-removal", want: false},
	} {
		if _, err := s.CreateInstance(&Instance{UUID: tc.uuid, NodeID: node.ID, State: tc.state, DesiredStateJSON: makeSpec(tc.uuid)}); err != nil {
			t.Fatalf("create %s: %v", tc.uuid, err)
		}
	}

	ds, err := s.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ds.RuntimePreference != proto.RuntimePreferenceDocker {
		t.Fatalf("runtime preference = %q", ds.RuntimePreference)
	}
	got := make(map[string]bool, len(ds.Instances))
	for _, spec := range ds.Instances {
		got[spec.UUID] = true
	}
	for _, tc := range []struct {
		uuid string
		want bool
	}{
		{uuid: "running", want: true},
		{uuid: "starting", want: true},
		{uuid: "pending", want: true},
		{uuid: "failed", want: false},
		{uuid: "stopped", want: false},
		{uuid: "removing", want: false},
	} {
		if got[tc.uuid] != tc.want {
			t.Errorf("instance %q in desired state = %v, want %v (desired=%+v)", tc.uuid, got[tc.uuid], tc.want, ds.Instances)
		}
	}
}

// The stored instance carries what names its image; where the node pulls it
// from is planned when the state is sent (hub.planReplay), so the store
// passes the instance through untouched. Registry logins ride along.
func TestDesiredStateForNodeCarriesImageFactsAndRegistryLogins(t *testing.T) {
	s := newTestStore(t)
	box, err := auth.NewSecretbox("desired-state-registry")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSecretbox(box)
	node, err := s.RegisterNode("pull-candidates-node", HashToken("token-candidates"), "candidate-host")
	if err != nil {
		t.Fatal(err)
	}
	stored := proto.InstanceDesiredState{
		UUID: "pull-candidates", Image: "gpustack/runner:cuda12.8-vllm0.17.1", DesiredState: "running",
		ImageSources:        []string{"quay.io/gpustack/runner:cuda12.8-vllm0.17.1", "gpustack/runner:cuda12.8-vllm0.17.1"},
		ImagePullCandidates: []proto.ImagePullCandidate{{Ref: "quay.io/gpustack/runner:cuda12.8-vllm0.17.1", Rank: true}},
	}
	spec, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance(&Instance{UUID: "pull-candidates", NodeID: node.ID, State: proto.InstStarting, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSourceCredential(0, "harbor.corp.example", "ci", "pw"); err != nil {
		t.Fatal(err)
	}
	ds, err := s.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Instances) != 1 || !reflect.DeepEqual(ds.Instances[0].ImageSources, stored.ImageSources) || !reflect.DeepEqual(ds.Instances[0].ImagePullCandidates, stored.ImagePullCandidates) {
		t.Fatalf("instances = %+v", ds.Instances)
	}
	if !reflect.DeepEqual(ds.RegistryAuth, []proto.SourceAuth{{Host: "harbor.corp.example", Username: "ci", Secret: "pw"}}) {
		t.Fatalf("registry auth = %+v", ds.RegistryAuth)
	}
}

func TestDesiredStateForNodeOrphanCleanupFollowsSetting(t *testing.T) {
	s := newTestStore(t)
	node, err := s.RegisterNode("orphan-cleanup-node", HashToken("token"), "orphan-host")
	if err != nil {
		t.Fatal(err)
	}
	ds, err := s.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ds.OrphanCleanup {
		t.Fatal("orphan cleanup must default to disabled when no setting row exists")
	}
	if err := s.SetSetting(SettingOrphanCleanup, "true"); err != nil {
		t.Fatal(err)
	}
	ds, err = s.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ds.OrphanCleanup {
		t.Fatal("orphan_cleanup=true was not carried into the desired state")
	}
	// A corrupt row must fail closed rather than enable a destructive sweep.
	if err := s.SetSetting(SettingOrphanCleanup, `{not-json`); err != nil {
		t.Fatal(err)
	}
	ds, err = s.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ds.OrphanCleanup {
		t.Fatal("a malformed orphan_cleanup row enabled the sweep")
	}
}
