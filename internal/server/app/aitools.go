package app

import (
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The assistant's tools read the platform API as the asking user
// (aihelper.APIReader); these are the findings the API does not answer on
// its own, and the reads the task seeds share.

func (a *aiService) NodeProfile(nodeUUID string) (any, error) {
	n, err := a.store.NodeWithGPUs(nodeUUID)
	if err != nil {
		return nil, err
	}
	owned := n.OwnedGPUs()
	gpus := make([]map[string]any, 0, len(owned))
	for _, g := range owned {
		arch := n.ArchOf(g)
		gpus = append(gpus, map[string]any{
			"index": g.Index, "name": g.Name, "memory_gib": g.MemoryTotal / (1 << 30),
			"arch": arch.Family, "arch_id": arch.ID, "compute_capability": g.ComputeCapability,
		})
	}
	return map[string]any{
		"hostname": n.Hostname, "cpu": n.CPUModel, "cpu_cores": n.CPUCores,
		"mem_gib": n.MemTotalKB / (1 << 20), "driver": firstNonEmpty(n.GPUDriver, n.NvidiaDriver), "gpus": gpus,
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (a *aiService) GPUTopology(nodeUUID string) (any, error) {
	n, err := a.store.NodeByUUID(nodeUUID)
	if err != nil {
		return nil, err
	}
	return a.store.NodeTopology(n.ID), nil
}

// instanceLaunchCommand reads the entrypoint and argv the instance was created
// with out of its desired state snapshot. A snapshot that cannot be parsed
// yields an empty command: the helper degrades to less context, never to
// invented context.
func instanceLaunchCommand(in store.Instance) ([]string, string) {
	if in.DesiredStateJSON == "" {
		return nil, ""
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(in.DesiredStateJSON), &spec); err != nil {
		return nil, ""
	}
	command := append(append([]string{}, spec.Entrypoint...), spec.Argv...)
	if len(command) == 0 {
		return nil, spec.CommandMode
	}
	return command, spec.CommandMode
}

func (a *aiService) ModelMeta(modelID uint) (any, error) {
	m, err := a.store.GetModel(modelID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"name": m.Name, "arch": m.Arch, "source": m.Source, "source_id": m.SourceID,
		"weight_gib": m.WeightBytes / (1 << 30), "dtype": m.Dtype,
	}, nil
}

// CompareWithHealthyReplica sets a failed instance beside a running replica
// of its deployment: when only some replicas fail, what differs is the lead.
func (a *aiService) CompareWithHealthyReplica(instanceUUID string) (any, error) {
	in, err := a.store.InstanceByUUID(instanceUUID)
	if err != nil {
		return nil, err
	}
	insts, err := a.store.InstancesForDeployment(in.DeploymentID)
	if err != nil {
		return nil, err
	}
	var healthy *store.Instance
	for i := range insts {
		if insts[i].UUID != in.UUID && insts[i].State == proto.InstRunning {
			healthy = &insts[i]
			break
		}
	}
	if healthy == nil {
		return map[string]any{"healthy_replica": nil, "note": "no running replica of this deployment to compare with"}, nil
	}
	failedCmd, _ := instanceLaunchCommand(*in)
	healthyCmd, _ := instanceLaunchCommand(*healthy)
	return map[string]any{
		"failed":          a.replicaFacts(in, failedCmd),
		"healthy_replica": a.replicaFacts(healthy, healthyCmd),
		"only_in_failed":  argDiff(failedCmd, healthyCmd),
		"only_in_healthy": argDiff(healthyCmd, failedCmd),
	}, nil
}

func (a *aiService) replicaFacts(in *store.Instance, command []string) map[string]any {
	node := ""
	if n, err := a.store.NodeByID(in.NodeID); err == nil {
		node = n.Hostname + " (" + n.UUID + ")"
	}
	return map[string]any{"instance_uuid": in.UUID, "state": in.State, "node": node, "gpu_indexes": instanceGPUs(*in), "image": in.Image, "command": command}
}

// argDiff lists the arguments of a that b does not have, in a's order.
func argDiff(a, b []string) []string {
	have := make(map[string]int, len(b))
	for _, s := range b {
		have[s]++
	}
	var out []string
	for _, s := range a {
		if have[s] > 0 {
			have[s]--
			continue
		}
		out = append(out, s)
	}
	return out
}

func instanceGPUs(in store.Instance) []int {
	var gpus []int
	if in.GPUIndexes != "" {
		_ = json.Unmarshal([]byte(in.GPUIndexes), &gpus)
	}
	return gpus
}

// askEnv is one ask's aihelper.ToolEnv: the service's own findings and the
// platform API as the user who asked reads it.
type askEnv struct {
	*aiService
	api aihelper.APIReader
}

func (e askEnv) API() aihelper.APIReader { return e.api }

// ToolEnv is the tools' environment for a reader of the platform API other
// than an ask (the MCP endpoint).
func (a *aiService) ToolEnv(api aihelper.APIReader) aihelper.ToolEnv {
	return askEnv{aiService: a, api: api}
}
