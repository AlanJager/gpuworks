// Command seeddemo inserts a realistic multi-GPU node + deployment into a
// gpuworks database so the topology graph and VRAM-breakdown UI can be exercised
// without real hardware. Dev tool only.
package main

import (
	"encoding/json"
	"flag"
	"log"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/topoparse"
)

func main() {
	db := flag.String("db", "./data/gpuworks.db", "sqlite path")
	flag.Parse()
	st, err := store.Open(*db, bus.New())
	if err != nil {
		log.Fatal(err)
	}
	if err := app.SyncInferenceCatalog(st); err != nil {
		log.Fatal(err)
	}
	g := st.DB()

	// 8x H100 node, two NVLink cliques of 4 across two NUMA nodes.
	topo := topoparse.Topology{
		GPUs:          make([]topoparse.GPU, 8),
		Matrix:        make([][]topoparse.Link, 8),
		NVLinkCliques: [][]int{{0, 1, 2, 3}, {4, 5, 6, 7}},
		NUMAGroups:    map[int][]int{0: {0, 1, 2, 3}, 1: {4, 5, 6, 7}},
	}
	for i := 0; i < 8; i++ {
		topo.GPUs[i] = topoparse.GPU{Index: i, NUMANode: i / 4, CPUAffinity: "0-47"}
		topo.Matrix[i] = make([]topoparse.Link, 8)
		for j := 0; j < 8; j++ {
			switch {
			case i == j:
				topo.Matrix[i][j] = topoparse.Link{Raw: "X", Class: topoparse.LinkSelf}
			case i/4 == j/4:
				topo.Matrix[i][j] = topoparse.Link{Raw: "NV18", Class: topoparse.LinkNVLink, NVLinkBonds: 18}
			default:
				topo.Matrix[i][j] = topoparse.Link{Raw: "SYS", Class: topoparse.LinkSYS}
			}
		}
	}
	topoJSON, _ := json.Marshal(topo)

	node := &store.Node{
		UUID: "demo-dgx-8xh100", State: store.NodeStateOnline, Hostname: "dgx-a",
		OS: "linux", Arch: "amd64", Kernel: "6.5.0", CPUModel: "AMD EPYC 9654", CPUCores: 96,
		MemTotalKB: 2 << 30, DiskTotalKB: 8 << 30, GPUCount: 8, NvidiaDriver: "550.90.07",
		TopoJSON: string(topoJSON),
	}
	g.Create(node)
	for i := 0; i < 8; i++ {
		g.Create(&store.GPU{
			NodeID: node.ID, Index: i, UUID: "GPU-demo-" + string(rune('a'+i)),
			Name: "H100 80GB HBM3", ComputeCapability: "9.0", Vendor: "nvidia",
			MemoryTotal: 80 << 30, NUMANode: i / 4,
		})
	}

	cfg := `{"architectures":["Qwen2ForCausalLM"],"model_type":"qwen2","num_hidden_layers":64,"hidden_size":5120,"num_attention_heads":40,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":32768,"vocab_size":152064,"torch_dtype":"bfloat16"}`
	model := &store.Model{Source: "modelscope", SourceID: "Qwen/Qwen2.5-32B-Instruct", Revision: "master",
		Name: "Qwen2.5-32B-Instruct", Arch: "Qwen2ForCausalLM", WeightBytes: 65 << 30, Dtype: "bfloat16",
		Meta: cfg}
	g.Create(model)

	dep := &store.Deployment{Name: "qwen32b", ModelID: model.ID, NodeID: node.ID, ServedName: "qwen2.5-32b",
		State: "active", Replicas: 1}
	g.Create(dep)

	explain := map[string]any{
		"explain": map[string]any{
			"chosen": map[string]any{"gpus": []int{0, 1, 2, 3}, "tp": 4, "per_gpu_bytes": int64(23) << 30, "score": 40,
				"reasons": []map[string]any{
					{"code": "topology", "message": "tp=4 on a fully NVLink-interconnected GPU group", "score": 30},
					{"code": "packing", "message": "binpack: prefer fewest GPUs", "score": -150},
					{"code": "tight_fit", "message": "capacity closely matches requirement", "score": 3},
				}},
			"topo_highlight": map[string]any{"gpus": []int{0, 1, 2, 3}, "numa": []int{0},
				"links": [][2]int{{0, 1}, {0, 2}, {0, 3}, {1, 2}, {1, 3}, {2, 3}}},
			"rejected": []map[string]any{{"tp": 1, "reason": "need 71.8 GiB/GPU for tp=1, only 0 GPU(s) large enough"}},
		},
		"breakdown": map[string]any{
			"weight_bytes": int64(65) << 30, "kv_bytes_per_token": 131072.0, "kv_total_bytes": int64(16) << 30,
			"reserve_bytes": int64(1536) << 20,
			"per_gpu_bytes": int64(23) << 30, "total_bytes": int64(92) << 30, "grade": "exact", "tensor_parallel": 4, "max_model_len": 32768,
		},
	}
	explJSON, _ := json.Marshal(explain)
	measured, _ := json.Marshal(map[string]any{"ModelWeightsGiB": 16.1, "KVCacheGiB": 3.9, "KVCacheTokens": 262144,
		"GraphCaptureGiB": 0.58, "MaxConcurrency": 8.0, "NumGPUBlocks": 16384, "Found": true})
	desired, _ := json.Marshal(proto.InstanceDesiredState{
		ResolvedLaunch: proto.ResolvedLaunch{
			CommandMode: string(deploy.CommandEngine),
			Entrypoint:  []string{"vllm", "serve", "/model"},
			Argv:        []string{"--served-model-name", "qwen2.5-32b", "--host", "0.0.0.0", "--port", "30000", "--tensor-parallel-size", "4", "--max-model-len", "32768"},
		},
		UUID: "demo-inst-1", DeploymentID: dep.ID, DesiredState: "running", Engine: "vllm",
		Image: "vllm/vllm-openai:latest", GPUIndexes: []int{0, 1, 2, 3}, Ports: []int{30000}, APIPort: 30000,
	})
	g.Create(&store.Instance{UUID: "demo-inst-1", DeploymentID: dep.ID, NodeID: node.ID, State: "running",
		GPUIndexes: "[0,1,2,3]", Ports: "[30000]", APIPort: 30000, Image: "vllm/vllm-openai:latest",
		DesiredStateJSON: string(desired), ScheduleExpl: string(explJSON), Measured: string(measured), DirectAddr: "127.0.0.1:30000"})

	log.Println("seeded demo node + deployment into", *db)
}
