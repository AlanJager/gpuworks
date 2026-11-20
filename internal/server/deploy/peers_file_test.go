package deploy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
)

var peersCallableArgs = []string{"--model", "${MODEL_PATH}", "--port", "${API_PORT}", "--rank", "${NODE_RANK}", "--nnodes", "${NNODES}", "--master-addr", "${MASTER_ADDR}", "--master-port", "${MASTER_PORT}", "--node-addr", "${NODE_ADDR}"}

func peersRanks() []proto.PeerRank {
	return []proto.PeerRank{
		{NodeRank: 0, NodeName: "gb10-1", NodeUUID: "n1", Hostname: "gb10-1", NodeAddr: "192.168.177.51", Management: &proto.ManagementAddress{Interface: "eno1", IP: "10.0.0.51"}, Interface: "enp1s0f1np1",
			HCAs: []proto.PeerHCA{{Name: "rocep1s0f1:1", Device: "rocep1s0f1", Port: 1, NetDev: "enp1s0f1np1", IP: "192.168.177.51"}, {Name: "roceP2p1s0f1:1", Device: "roceP2p1s0f1", Port: 1, NetDev: "enP2p1s0f1np1", IP: "192.168.178.51"}}},
		{NodeRank: 1, NodeName: "gb10-2", NodeUUID: "n2", Hostname: "gb10-2", NodeAddr: "192.168.177.52", Management: &proto.ManagementAddress{Interface: "eno1", IP: "10.0.0.52"}, Interface: "enp1s0f1np1",
			HCAs: []proto.PeerHCA{{Name: "rocep1s0f1:1", Device: "rocep1s0f1", Port: 1, NetDev: "enp1s0f1np1", IP: "192.168.177.52"}, {Name: "roceP2p1s0f1:1", Device: "roceP2p1s0f1", Port: 1, NetDev: "enP2p1s0f1np1", IP: "192.168.178.52"}}},
	}
}

func peersParams(rank int) Params {
	return Params{
		ModelPath: "/model", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 2, TemplateArgs: peersCallableArgs,
		NNodes: 2, NodeRank: rank, Headless: rank > 0, MasterAddr: "192.168.177.51", MasterPort: 29500, NodeAddress: peersRanks()[rank].NodeAddr,
		ComputeNetwork: ComputeNetwork{Type: netplan.TypeHostRDMA, Interface: "enp1s0f1np1", HCAs: []string{"rocep1s0f1:1", "roceP2p1s0f1:1"}, LinkLayer: netplan.LinkLayerRoCE},
		Peers:          peersRanks(),
	}
}

// A rank of a multi-node group launches with the group's peers file and the
// variable that names it; the document is the same on every rank except the
// reader's own rank.
func TestMultiNodeRankLaunchCarriesItsPeersFile(t *testing.T) {
	for _, rank := range []int{0, 1} {
		launch, err := callableLaunch(t, peersParams(rank), ResolveLaunchInput{})
		if err != nil {
			t.Fatal(err)
		}
		want := &proto.PeersFile{Version: 1, NodeRank: rank, NNodes: 2, MasterAddr: "192.168.177.51", MasterPort: 29500,
			Network: &proto.PeersNetwork{Type: "host_rdma", LinkLayer: "roce"}, Ranks: peersRanks()}
		if !reflect.DeepEqual(launch.Peers, want) {
			t.Fatalf("rank %d peers = %+v, want %+v", rank, launch.Peers, want)
		}
		if launch.Env[PeersFileEnv] != proto.PeersFileMountPath || launch.EnvLayers.System[PeersFileEnv] != proto.PeersFileMountPath {
			t.Fatalf("rank %d env = %v system = %v", rank, launch.Env, launch.EnvLayers.System)
		}
	}
}

// Every field that is also a launch variable equals that variable on the
// rank: what a template expands ${NODE_RANK}, ${NNODES}, ${MASTER_ADDR},
// ${MASTER_PORT} and ${NODE_ADDR} to is what the reader finds in its file.
func TestPeersFileAgreesWithTheLaunchVariables(t *testing.T) {
	for _, rank := range []int{0, 1} {
		launch, err := callableLaunch(t, peersParams(rank), ResolveLaunchInput{})
		if err != nil {
			t.Fatal(err)
		}
		flag := func(name string) string {
			for i, arg := range launch.Argv {
				if arg == name && i+1 < len(launch.Argv) {
					return launch.Argv[i+1]
				}
			}
			t.Fatalf("argv %v has no %s", launch.Argv, name)
			return ""
		}
		file := launch.Peers
		got := map[string]string{
			"NODE_RANK": strconv.Itoa(file.NodeRank), "NNODES": strconv.Itoa(file.NNodes),
			"MASTER_ADDR": file.MasterAddr, "MASTER_PORT": strconv.Itoa(file.MasterPort), "NODE_ADDR": file.Ranks[file.NodeRank].NodeAddr,
		}
		want := map[string]string{
			"NODE_RANK": flag("--rank"), "NNODES": flag("--nnodes"),
			"MASTER_ADDR": flag("--master-addr"), "MASTER_PORT": flag("--master-port"), "NODE_ADDR": flag("--node-addr"),
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("rank %d peers file %v, launch variables %v", rank, got, want)
		}
	}
	// The reader's own row states its NODE_ADDR even when the placement's
	// list said otherwise.
	params := peersParams(1)
	params.NodeAddress = "192.168.177.99"
	launch, err := callableLaunch(t, params, ResolveLaunchInput{})
	if err != nil || launch.Peers.Ranks[1].NodeAddr != "192.168.177.99" {
		t.Fatalf("own node_addr = %+v err=%v", launch.Peers, err)
	}
}

// The document a reader parses is exactly this wire shape.
func TestPeersFileWireShape(t *testing.T) {
	launch, err := callableLaunch(t, peersParams(1), ResolveLaunchInput{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(launch.Peers)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"node_rank":1,"nnodes":2,"master_addr":"192.168.177.51","master_port":29500,"network":{"type":"host_rdma","link_layer":"roce"},"ranks":[` +
		`{"node_rank":0,"node_uuid":"n1","node_name":"gb10-1","hostname":"gb10-1","node_addr":"192.168.177.51","management":{"interface":"eno1","ip":"10.0.0.51"},"interface":"enp1s0f1np1","hcas":[{"name":"rocep1s0f1:1","device":"rocep1s0f1","port":1,"netdev":"enp1s0f1np1","ip":"192.168.177.51"},{"name":"roceP2p1s0f1:1","device":"roceP2p1s0f1","port":1,"netdev":"enP2p1s0f1np1","ip":"192.168.178.51"}]},` +
		`{"node_rank":1,"node_uuid":"n2","node_name":"gb10-2","hostname":"gb10-2","node_addr":"192.168.177.52","management":{"interface":"eno1","ip":"10.0.0.52"},"interface":"enp1s0f1np1","hcas":[{"name":"rocep1s0f1:1","device":"rocep1s0f1","port":1,"netdev":"enp1s0f1np1","ip":"192.168.177.52"},{"name":"roceP2p1s0f1:1","device":"roceP2p1s0f1","port":1,"netdev":"enP2p1s0f1np1","ip":"192.168.178.52"}]}]}`
	if string(raw) != want {
		t.Fatalf("peers file wire =\n%s\nwant\n%s", raw, want)
	}
	var back proto.PeersFile
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(&back, launch.Peers) {
		t.Fatalf("round trip = %+v err=%v", back, err)
	}
}

// A launch that is not a rank of a multi-node group, one whose placement
// stated no peers (an agent that does not write the file), one whose peers do
// not cover the group, and the symbolic command a manual script starts from
// all name no peers file.
func TestNoPeersFileOutsideAMultiNodeRank(t *testing.T) {
	single := Params{ModelPath: "/model", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 1, TemplateArgs: []string{"--model", "${MODEL_PATH}", "--port", "${API_PORT}"}, Peers: peersRanks()}
	noPeers := peersParams(0)
	noPeers.Peers = nil
	short := peersParams(0)
	short.Peers = short.Peers[:1]
	symbolic := peersParams(0)
	symbolic.Symbolic = true
	for name, params := range map[string]Params{"single node": single, "no peers": noPeers, "peers short of the group": short, "symbolic": symbolic} {
		t.Run(name, func(t *testing.T) {
			launch, err := callableLaunch(t, params, ResolveLaunchInput{AllowUnresolved: params.Symbolic})
			if err != nil {
				t.Fatal(err)
			}
			if launch.Peers != nil {
				t.Fatalf("peers = %+v", launch.Peers)
			}
			if _, set := launch.Env[PeersFileEnv]; set {
				t.Fatalf("env names a peers file: %v", launch.Env)
			}
		})
	}
}

// The environment variable is a system-layer default: a template or user
// value wins, as for every other system key.
func TestPeersFileEnvYieldsToTemplateAndUser(t *testing.T) {
	params := peersParams(0)
	params.TemplateEnv = map[string]string{PeersFileEnv: "/etc/fleet/peers.json"}
	launch, err := callableLaunch(t, params, ResolveLaunchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env[PeersFileEnv] != "/etc/fleet/peers.json" || launch.Peers == nil {
		t.Fatalf("env = %v peers = %v", launch.Env, launch.Peers != nil)
	}
}

// ${GPUWORKS_PEERS_FILE} is a launch variable like ${GPUWORKS_NODE_RANK}: a
// template field and a manual command both expand it to the path, and a
// launch that has no peers file (a single node) refuses a reference to it
// rather than handing the recipe an empty string.
func TestPeersFileIsALaunchVariable(t *testing.T) {
	script := "python3 /opt/pico/fleet.py --peers ${GPUWORKS_PEERS_FILE} --rank ${GPUWORKS_NODE_RANK}\nexec dsfront --port 19001"
	launch, err := callableLaunch(t, peersParams(0), ResolveLaunchInput{ContainerOverrides: ContainerOverrides{CommandMode: "manual", CmdDisplay: script}})
	if err != nil {
		t.Fatal(err)
	}
	if len(launch.Argv) != 2 || !strings.Contains(launch.Argv[1], "--peers "+proto.PeersFileMountPath+" --rank 0") {
		t.Fatalf("argv = %q", launch.Argv)
	}
	if launch.Env[PeersFileEnv] != proto.PeersFileMountPath || launch.Peers == nil {
		t.Fatalf("env = %v peers = %v", launch.Env, launch.Peers != nil)
	}

	params := peersParams(1)
	params.TemplateArgs = append(append([]string(nil), peersCallableArgs...), "--peers", "${GPUWORKS_PEERS_FILE}")
	params.TemplateEnv = map[string]string{"FLEET_PEERS": "${PEERS_FILE}"}
	launch, err = callableLaunch(t, params, ResolveLaunchInput{})
	if err != nil {
		t.Fatal(err)
	}
	if launch.Argv[len(launch.Argv)-1] != proto.PeersFileMountPath || launch.Env["FLEET_PEERS"] != proto.PeersFileMountPath {
		t.Fatalf("argv = %q env = %v", launch.Argv, launch.Env)
	}

	single := Params{ModelPath: "/model", APIPort: 19001, AssignedGPUs: 1, TensorParallel: 1, TemplateArgs: []string{"--model", "${MODEL_PATH}", "--port", "${API_PORT}", "--peers", "${GPUWORKS_PEERS_FILE}"}}
	if _, err := callableLaunch(t, single, ResolveLaunchInput{}); err == nil || !strings.Contains(err.Error(), "PEERS_FILE") {
		t.Fatalf("a single-node reference to the peers file = %v, want it refused", err)
	}
}

// The platform keeps /gpuworks for the files it provides: a new launch may
// not mount there or above, with or without a peers file. A deployment as
// stored keeps a mount it was created with there, unless it would now cover
// the peers file it gets.
func TestPlatformFileDirIsReserved(t *testing.T) {
	mount := func(target string) ResolveLaunchInput {
		return ResolveLaunchInput{ContainerOverrides: ContainerOverrides{
			ExtraMounts: []ContainerMount{{HostPath: "/srv/fleet", ContainerPath: target, ReadOnly: true}},
		}}
	}
	single := peersParams(0)
	single.Peers = nil
	for _, target := range []string{proto.PeersFileMountPath, "/gpuworks", "/gpuworks/cache", "/gpuworks/pre-start.sh"} {
		for name, params := range map[string]Params{"with peers": peersParams(0), "without peers": single} {
			if _, err := callableLaunch(t, params, mount(target)); err == nil || !strings.Contains(err.Error(), proto.PlatformFileDir) {
				t.Fatalf("%s: mount at %s err = %v, want it refused", name, target, err)
			}
		}
	}
	_, err := callableLaunch(t, peersParams(0), mount("/gpuworks/cache"))
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorMountTargetReserved ||
		product.Params()["target"] != "/gpuworks/cache" || product.Params()["reserved"] != proto.PlatformFileDir {
		t.Fatalf("reserved mount err = %v, want %s naming the target and %s", err, messages.ErrorMountTargetReserved, proto.PlatformFileDir)
	}
	if _, err := callableLaunch(t, peersParams(0), mount("/gpuworks-data")); err != nil {
		t.Fatalf("a path beside the platform's directory was refused: %v", err)
	}

	stored := func(target string) ResolveLaunchInput {
		input := mount(target)
		input.StoredLaunch = true
		return input
	}
	if _, err := callableLaunch(t, single, stored("/gpuworks/cache")); err != nil {
		t.Fatalf("a stored launch lost the mount it was created with: %v", err)
	}
	if _, err := callableLaunch(t, peersParams(0), stored("/gpuworks/cache")); err != nil {
		t.Fatalf("a stored mount beside the peers file was refused: %v", err)
	}
	for _, target := range []string{proto.PeersFileMountPath, "/gpuworks"} {
		if _, err := callableLaunch(t, peersParams(0), stored(target)); err == nil {
			t.Fatalf("a stored mount at %s that covers the peers file was kept", target)
		}
	}
}
