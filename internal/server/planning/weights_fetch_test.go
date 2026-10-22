package planning

import (
	"errors"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/sourceurl"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func weightsRulesStore(t *testing.T, name string) (*store.Store, *store.Node, *store.Node) {
	t.Helper()
	st := testImagePullStore(t, name)
	idc := registerLabelled(t, st, "idc-node", map[string]string{"site": "idc-a"})
	office := registerLabelled(t, st, "office-node", map[string]string{"site": "office"})
	for _, spec := range []store.PullSourceRuleSpec{
		// The data center reaches only its own proxy.
		{Match: "https://huggingface.co", Endpoints: []string{"http://hf-proxy.corp:8090"}, AllowUpstream: false, Requirements: nodelabel.Requirements{"site": {"idc-a"}}},
		{Match: "docker.io", Endpoints: []string{"harbor.corp.example/dockerhub-proxy"}, AllowUpstream: true},
		{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true},
		// Offline for ModelScope in the data center.
		{Match: "https://modelscope.cn", AllowUpstream: false, Requirements: nodelabel.Requirements{"site": {"idc-a"}}},
	} {
		if _, err := st.CreatePullSourceRule(spec); err != nil {
			t.Fatal(err)
		}
	}
	return st, idc, office
}

func TestWeightsFetchCandidatesFollowTheNodesSiteRules(t *testing.T) {
	st, idc, office := weightsRulesStore(t, "planning_weights_fetch")
	plan := func(node *store.Node, source, sourceURL string) ([]proto.ModelFetchCandidate, error) {
		rules, err := RulesFor(st, nil, node)
		if err != nil {
			t.Fatal(err)
		}
		return WeightsFetchCandidatesFor(URLRules(rules), node, source, sourceURL, "Qwen/Qwen3-8B")
	}
	got, err := plan(idc, "huggingface", "")
	want := []proto.ModelFetchCandidate{{Base: "http://hf-proxy.corp:8090"}, {Base: "https://hf-mirror.com"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("idc = %+v, %v; want %+v (its proxy, then the global mirror, upstream forbidden)", got, err, want)
	}
	got, err = plan(office, "huggingface", "")
	want = []proto.ModelFetchCandidate{{Base: "https://hf-mirror.com"}, {Base: "https://huggingface.co", Upstream: true}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("office = %+v, %v; want %+v", got, err, want)
	}
	got, err = plan(office, "modelscope", "")
	if err != nil || !reflect.DeepEqual(got, []proto.ModelFetchCandidate{{Base: "https://modelscope.cn", Upstream: true}}) {
		t.Fatalf("office modelscope = %+v, %v", got, err)
	}
	// An HTTP directory is where its bytes are: no rule moves it.
	got, err = plan(idc, "url", "https://huggingface.co/files/qwen/")
	if err != nil || !reflect.DeepEqual(got, []proto.ModelFetchCandidate{{Base: "https://huggingface.co/files/qwen", Upstream: true}}) {
		t.Fatalf("directory = %+v, %v", got, err)
	}
	if _, err := plan(idc, "local", ""); err == nil {
		t.Fatal("a source with no address must not plan a download")
	}

	_, err = plan(idc, "modelscope", "")
	var product *messages.ProductError
	if !errors.Is(err, sourceurl.ErrNoAllowedSource) || !errors.As(err, &product) || product.Code() != messages.ErrorWeightsFetchForbidden ||
		product.Params()["node"] != idc.DisplayName() || product.Params()["model"] != "Qwen/Qwen3-8B" || product.Params()["route"] != "/api/v1/pull/source-rules" {
		t.Fatalf("forbidden = %#v", err)
	}
	reason, ok := WeightsFetchRefusal(err, idc, "Qwen3 8B")
	if !ok || !reflect.DeepEqual(reason, scheduler.WeightsFetchForbidden(idc.DisplayName(), "Qwen3 8B")) {
		t.Fatalf("refusal = %+v, %v", reason, ok)
	}
	if _, ok := WeightsFetchRefusal(errors.New("store unavailable"), idc, "x"); ok {
		t.Fatal("only a forbidden plan is a refusal")
	}
}

// Only downloads are planned; a forbidden download leaves the replay and the
// rest of the node's tasks go out as they are.
func TestPlanModelFileTasksPlansDownloadsAndLeavesForbiddenOnesOut(t *testing.T) {
	st, idc, _ := weightsRulesStore(t, "planning_weights_fetch_tasks")
	rules, err := RulesFor(st, nil, idc)
	if err != nil {
		t.Fatal(err)
	}
	tasks := []proto.ModelFileTask{
		{Key: "1", Source: "huggingface", SourceID: "org/a", Acquisition: proto.ModelAcquireDownload},
		{Key: "2", Source: "modelscope", SourceID: "org/b", Acquisition: proto.ModelAcquireDownload},
		{Key: "3", Source: "modelscope", SourceID: "org/c", Acquisition: proto.ModelAcquirePeer},
		{Key: "4", Source: "url", SourceID: "files/d", Endpoint: "http://files.corp/d/", Acquisition: proto.ModelAcquireDownload},
	}
	planned, refused := PlanModelFileTasks(URLRules(rules), idc, tasks)
	if len(refused) != 1 || !errors.Is(refused[0], sourceurl.ErrNoAllowedSource) {
		t.Fatalf("refused = %v", refused)
	}
	if len(planned) != 3 || planned[0].Key != "1" || planned[1].Key != "3" || planned[2].Key != "4" {
		t.Fatalf("planned = %+v", planned)
	}
	if planned[0].Endpoint != "http://hf-proxy.corp:8090" || len(planned[0].Candidates) != 2 {
		t.Fatalf("download = %+v", planned[0])
	}
	if planned[1].Candidates != nil || planned[1].Endpoint != "" {
		t.Fatalf("a peer copy is not planned: %+v", planned[1])
	}
	if planned[2].Endpoint != "http://files.corp/d" || !reflect.DeepEqual(planned[2].Candidates, []proto.ModelFetchCandidate{{Base: "http://files.corp/d", Upstream: true}}) {
		t.Fatalf("directory = %+v", planned[2])
	}
	if tasks[0].Candidates != nil {
		t.Fatal("the caller's tasks were changed in place")
	}

	// A task pushed on its own reads the same rules and gets the same plan.
	one := tasks[0]
	if err := PlanModelFileTask(st, nil, idc, &one, "org/a"); err != nil || !reflect.DeepEqual(one, planned[0]) {
		t.Fatalf("pushed = %+v, %v; want the replay's %+v", one, err, planned[0])
	}
}

// The control plane reads through the site rules chosen for it, whatever
// nodes those rules select, and no other rule.
func TestServerFetchCandidatesReadTheRulesChosenForTheControlPlane(t *testing.T) {
	st := testImagePullStore(t, "planning_server_fetch")
	for _, spec := range []store.PullSourceRuleSpec{
		// The data center's proxy is not reachable from the management node.
		{Match: "https://huggingface.co", Endpoints: []string{"http://hf-proxy.corp:8090"}, AllowUpstream: false, Requirements: nodelabel.Requirements{"site": {"idc-a"}}},
		{Match: "https://huggingface.co", Endpoints: []string{"https://hf-mirror.com"}, AllowUpstream: true, ControlPlane: true, Requirements: nodelabel.Requirements{"site": {"office"}}},
		{Match: "https://modelscope.cn", AllowUpstream: false, ControlPlane: true},
	} {
		if _, err := st.CreatePullSourceRule(spec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ServerFetchCandidates(st, "huggingface", "")
	if err != nil || !reflect.DeepEqual(got, []string{"https://hf-mirror.com", "https://huggingface.co"}) {
		t.Fatalf("hugging face = %v, %v", got, err)
	}
	if _, err := ServerFetchCandidates(st, "modelscope", ""); !errors.Is(err, sourceurl.ErrNoAllowedSource) {
		t.Fatalf("forbidden modelscope = %v", err)
	}
	got, err = ServerFetchCandidates(st, "url", "http://files.corp/m/")
	if err != nil || !reflect.DeepEqual(got, []string{"http://files.corp/m"}) {
		t.Fatalf("directory = %v, %v", got, err)
	}
	if _, err := ServerFetchCandidates(st, "local", ""); err == nil {
		t.Fatal("a local model has nothing to read remotely")
	}
}

// A node uses the first proxy entry that selects it - entries do not add up
// as rules do - and none when no entry selects it. The management node uses
// the first entry chosen for it, whatever nodes that entry selects.
func TestPullSettingsForPicksTheFirstProxyThatSelectsTheNode(t *testing.T) {
	st, idc, office := weightsRulesStore(t, "planning_pull_proxy")
	box, err := auth.NewSecretbox("planning-proxy")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	if proxy, err := ControlPlaneProxy(st); err != nil || proxy != nil {
		t.Fatalf("no entries: control plane proxy = %+v, %v", proxy, err)
	}
	for _, entry := range []struct {
		spec     store.PullProxySpec
		password string
	}{
		{store.PullProxySpec{HTTPProxy: "http://idc-proxy:3128", Username: "alice", Requirements: nodelabel.Requirements{"site": {"idc-a"}}}, "pw"},
		{store.PullProxySpec{HTTPSProxy: "http://office-proxy:3128", NoProxy: "files.corp", ControlPlane: true, Requirements: nodelabel.Requirements{"site": {"office"}}}, ""},
		{store.PullProxySpec{HTTPProxy: "http://never:3128", Requirements: nodelabel.Requirements{"site": {"idc-a"}}}, ""},
	} {
		if _, err := st.CreatePullProxy(entry.spec, entry.password); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := PullSettingsFor(st, nil, idc)
	if err != nil || selection.Proxy == nil || selection.Proxy.HTTPProxy != "http://idc-proxy:3128" || len(selection.Rules) != 4 {
		t.Fatalf("idc = %+v, %v", selection, err)
	}
	if got := selection.Network(); !reflect.DeepEqual(got, proto.NetworkConfig{HTTPProxy: "http://alice:pw@idc-proxy:3128"}) {
		t.Fatalf("idc network = %+v, want the entry's address with its login", got)
	}
	selection, err = PullSettingsFor(st, nil, office)
	if err != nil || selection.Proxy == nil || selection.Network().NoProxy != "files.corp" {
		t.Fatalf("office = %+v, %v", selection, err)
	}
	other := registerLabelled(t, st, "elsewhere", map[string]string{"site": "lab"})
	if selection, err = PullSettingsFor(st, nil, other); err != nil || selection.Proxy != nil || !reflect.DeepEqual(selection.Network(), proto.NetworkConfig{}) {
		t.Fatalf("a node no entry selects = %+v, %v", selection, err)
	}
	proxy, err := ControlPlaneProxy(st)
	if err != nil || proxy == nil || proxy.HTTPSProxy != "http://office-proxy:3128" {
		t.Fatalf("control plane proxy = %+v, %v", proxy, err)
	}
	if network, err := ControlPlaneNetwork(st); err != nil || network.HTTPSProxy != "http://office-proxy:3128" {
		t.Fatalf("control plane network = %+v, %v", network, err)
	}
}
