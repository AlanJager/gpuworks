package deploy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestPDMultiNodeDecodeServesAndRebuildsAsOneGroup runs a decode instance
// that spans two nodes (plan stage 4c) through the real API, controller, two
// agents and the gateway: the leader and its headless follower meet on the
// leader's collective port, the gateway sends the decode stage to the leader
// only, and when the follower's engine dies the unit is rebuilt as a new
// group while the prefill instance keeps running.
func TestPDMultiNodeDecodeServesAndRebuildsAsOneGroup(t *testing.T) {
	env := testenv.New(t)
	const leaderAddress, followerAddress = "10.43.0.10", "10.43.0.11"
	a := env.AddNode("pd-rank-0", testenv.WithGPUs(2, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("fake"), testenv.WithManagementAddress(leaderAddress))
	b := env.AddNode("pd-rank-1", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}),
		testenv.WithDriverName("fake"), testenv.WithManagementAddress(followerAddress))
	followerRuns := make(chan runtime.WorkloadSpec, 4)
	b.Driver.AfterRun(func(spec runtime.WorkloadSpec) { captureWorkload(followerRuns, spec) })
	env.WaitNodeOnline(t, a.Name)
	env.WaitNodeOnline(t, b.Name)
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "zstackai/qwen3-pd-group-fake", Revision: "main", Name: "qwen3-pd-group-fake", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"],"num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*testenv.Node{a, b} {
		row, err := env.Store().NodeByUUID(node.UUID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: row.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "qwen3-pd-group-fake")}); err != nil {
			t.Fatal(err)
		}
	}
	api := env.API(t)
	payload, _ := json.Marshal(map[string]any{
		"model_id": model.ID, "served_name": "pd-group", "engine": "vllm", "image": "gpuworks/fake-engine:test",
		"quick_params": map[string]any{"max_model_len": 512},
		"placement": map[string]any{"pd": map[string]any{"mode": "vllm-nixl", "roles": map[string]any{
			"prefill": map[string]any{"tp": 1, "units": []any{map[string]any{"node_uuid": a.UUID, "gpus": []int{0}}}},
			"decode": map[string]any{"tp": 2, "units": []any{map[string]any{"node_uuid": a.UUID, "gpus": []int{1},
				"followers": []any{map[string]any{"node_uuid": b.UUID}}}}},
		}}},
	})
	resp, err := api.PostJSON("/api/v1/deployments", payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	var created dto.Deployment
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	deployment := waitPDRunning(t, api, created.ID)
	prefill, leader, follower := pdGroupInstances(deployment)
	if prefill.UUID == "" || leader.UUID == "" || follower.UUID == "" {
		t.Fatalf("instances = %+v", deployment.Instances)
	}
	followerSpec := waitCapturedWorkload(t, followerRuns, "decode follower")
	argv := strings.Join(followerSpec.Cmd, " ")
	if !strings.Contains(argv, "--headless") || !strings.Contains(argv, "--node-rank 1") || !strings.Contains(argv, "--master-addr "+leaderAddress) || !strings.Contains(argv, "kv_consumer") {
		t.Fatalf("follower argv = %s", argv)
	}
	if followerSpec.Env["VLLM_NIXL_SIDE_CHANNEL_HOST"] != followerAddress {
		t.Fatalf("follower side channel host = %q", followerSpec.Env["VLLM_NIXL_SIDE_CHANNEL_HOST"])
	}

	key, err := env.Store().CreateAPIKey("pd-group", 0)
	if err != nil {
		t.Fatal(err)
	}
	chat := func() (int, string) {
		req, _ := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", bytes.NewReader([]byte(`{"model":"pd-group","messages":[{"role":"user","content":"hi"}]}`)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if status, body := chat(); status != http.StatusOK {
		t.Fatalf("chat = %d %s", status, body)
	}
	// The prefill instance shares node A with the decode leader, but the
	// follower's share of the KV leaves it: the check must not call it local.
	verified := waitHandoffEvent(t, env.Store(), prefill.UUID)
	if verified.Code != "PD_HANDOFF_VERIFIED" || !strings.Contains(verified.Data, `"transport":"tcp"`) {
		t.Fatalf("handoff event = %+v", verified)
	}

	// The follower's worker exits and its agent reports it: its unit is
	// rebuilt as a new group and the prefill instance is left alone.
	b.Driver.Exit(followerSpec.Name, 1)
	// The rebuild takes about 12 s on an idle machine and several times that
	// on a loaded CI runner.
	deadline := time.Now().Add(3 * time.Minute)
	var rebuilt dto.Deployment
	for time.Now().Before(deadline) {
		rebuilt = waitPDRunning(t, api, created.ID)
		p, l, f := pdGroupInstances(rebuilt)
		if l.UUID != "" && l.UUID != leader.UUID && f.UUID != "" && f.UUID != follower.UUID {
			if p.UUID != prefill.UUID {
				t.Fatalf("prefill was rebuilt with the decode group: %s -> %s", prefill.UUID, p.UUID)
			}
			if status, body := chat(); status != http.StatusOK {
				t.Fatalf("chat after rebuild = %d %s", status, body)
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("decode group was not rebuilt: %+v", rebuilt.Instances)
}

// pdGroupInstances picks the running prefill instance, decode leader and
// decode follower of a deployment.
func pdGroupInstances(d dto.Deployment) (prefill, leader, follower dto.Instance) {
	for _, inst := range d.Instances {
		if inst.State != proto.InstRunning {
			continue
		}
		switch {
		case inst.Role == "prefill":
			prefill = inst
		case inst.Role == "decode" && inst.NodeRank > 0:
			follower = inst
		case inst.Role == "decode":
			leader = inst
		}
	}
	return
}
