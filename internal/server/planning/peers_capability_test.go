package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The peers file follows the one agent capability rule: a template that
// references ${GPUWORKS_PEERS_FILE} (either spelling in its launch fields,
// the prefixed one in a manual command) keeps the peers and an older agent's
// node is refused; a launch that does not reference it goes without the file
// on an older agent, as a multi-node launch did before the file existed.
func TestThePeersFileGoesOnlyToAnAgentThatWritesIt(t *testing.T) {
	peers := []proto.PeerRank{{NodeRank: 0, NodeUUID: "n0"}, {NodeRank: 1, NodeUUID: "n1"}}
	older, current := []string{proto.CapNetworkUpdate}, []string{proto.CapNetworkUpdate, proto.CapPeersFile}
	request := func(caps []string, selection deploy.CatalogSelection, overrides deploy.ContainerOverrides) LaunchRequest {
		return LaunchRequest{AgentCaps: caps, Selection: selection, Topology: DistTopology{NNodes: 2, Peers: peers}, Source: LaunchSource{Overrides: overrides}}
	}
	none := deploy.CatalogSelection{ExtraArgs: []string{"--rank", "${GPUWORKS_NODE_RANK}"}}
	manual := func(script string) deploy.ContainerOverrides {
		return deploy.ContainerOverrides{CommandMode: string(deploy.CommandManual), CmdDisplay: script}
	}
	for name, test := range map[string]struct {
		req  LaunchRequest
		keep bool
	}{
		"current agent":             {request(current, none, deploy.ContainerOverrides{}), true},
		"agent not connected":       {request(nil, none, deploy.ContainerOverrides{}), true},
		"older agent, unreferenced": {request(older, none, deploy.ContainerOverrides{}), false},
		"older agent, args":         {request(older, deploy.CatalogSelection{ExtraArgs: []string{"--fleet", "${GPUWORKS_PEERS_FILE}"}}, deploy.ContainerOverrides{}), true},
		"older agent, bare alias":   {request(older, deploy.CatalogSelection{PreStartScript: "cp ${PEERS_FILE} /tmp/fleet.json"}, deploy.ContainerOverrides{}), true},
		"older agent, env":          {request(older, deploy.CatalogSelection{Env: map[string]string{"FLEET": "${GPUWORKS_PEERS_FILE}"}}, deploy.ContainerOverrides{}), true},
		"older agent, manual":       {request(older, none, manual("run --fleet ${GPUWORKS_PEERS_FILE}")), true},
		// A manual command expands only the prefixed name; the bare one is
		// the shell's own variable.
		"older agent, manual bare": {request(older, none, manual("run --fleet ${PEERS_FILE}")), false},
	} {
		if got := peersForAgent(test.req); (got != nil) != test.keep {
			t.Fatalf("%s: peers = %+v, want kept %v", name, got, test.keep)
		}
	}
	withPeers := deploy.ResolvedLaunch{Peers: &proto.PeersFile{Version: proto.PeersFileVersion}}
	refusal := agentCapabilityRefusal(older, &store.Node{Name: "gb10-3", UUID: "n3"}, withPeers)
	if refusal == nil || refusal.Code != scheduler.ReasonAgentCapabilityMissing || refusal.Params["capability"] != proto.CapPeersFile {
		t.Fatalf("refusal = %+v", refusal)
	}
	if refusal := agentCapabilityRefusal(current, &store.Node{Name: "gb10-3"}, withPeers); refusal != nil {
		t.Fatalf("current agent refused: %+v", refusal)
	}
}
