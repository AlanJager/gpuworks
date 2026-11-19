package proto

import (
	"encoding/json"
	"testing"
)

// The Slurm agent's report on the wire: the digests alone carry "nodes":
// null, a cluster with no nodes an empty list, and a node's listed_only and
// reason only when set. A renamed tag changes both sides at once, so only a
// literal pins what goes over the wire.
func TestSlurmReportWireJSON(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"digests alone", SlurmGPUNodeReport{Digest: "d", StateDigest: "s"}, `{"digest":"d","state_digest":"s","nodes":null}`},
		{"no nodes", SlurmGPUNodeReport{Digest: "d", StateDigest: "s", Nodes: []SlurmGPUNodeEntry{}}, `{"digest":"d","state_digest":"s","nodes":[]}`},
		{"nodes", SlurmGPUNodeReport{Digest: "d", Nodes: []SlurmGPUNodeEntry{
			{Name: "gpu0", State: "idle", GPUs: 2},
			{Name: "cpu0", ListedOnly: true},
			{Name: "gpu1", State: "drained", Reason: "disk", GPUs: 1},
		}}, `{"digest":"d","state_digest":"","nodes":[{"name":"gpu0","state":"idle","gpus":2},{"name":"cpu0","listed_only":true,"state":"","gpus":0},{"name":"gpu1","state":"drained","reason":"disk","gpus":1}]}`},
		{"answer", SlurmGPUNodeReportResponse{NeedNodes: true, Settings: SlurmAgentSettings{SbatchArgs: "-A ai", IdleReturnSeconds: 60, PendingTimeoutSeconds: 600, AccountUser: "svc"}},
			`{"need_nodes":true,"settings":{"sbatch_args":"-A ai","idle_return_seconds":60,"pending_timeout_seconds":600,"account_user":"svc"}}`},
		{"enroll", SlurmEnrollRequest{Token: "t", SlurmClusterName: "c", NodeName: "n"}, `{"token":"t","slurm_cluster_name":"c","node_name":"n"}`},
		{"enrolled", SlurmEnrollResponse{NodeUUID: "u", NodeToken: "k"}, `{"node_uuid":"u","node_token":"k","settings":{"sbatch_args":"","idle_return_seconds":0,"pending_timeout_seconds":0,"account_user":""}}`},
	}
	for _, c := range cases {
		raw, err := json.Marshal(c.in)
		if err != nil || string(raw) != c.want {
			t.Errorf("%s:\n got %s (%v)\nwant %s", c.name, raw, err, c.want)
		}
	}
	var back SlurmGPUNodeReport
	if err := json.Unmarshal([]byte(`{"digest":"d"}`), &back); err != nil || back.Nodes != nil {
		t.Errorf("an earlier agent's digests alone (no nodes key) = %+v, %v; want no list", back, err)
	}
	if err := json.Unmarshal([]byte(`{"digest":"d","nodes":[]}`), &back); err != nil || back.Nodes == nil || len(back.Nodes) != 0 {
		t.Errorf("an empty list = %+v, %v; want a list with nothing in it", back, err)
	}
}
