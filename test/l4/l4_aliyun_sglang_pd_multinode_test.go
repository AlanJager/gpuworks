//go:build cloudgpu && aliyun

package l4

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/testenv/goldenimage"
)

// The golden image carries the vLLM runner only; this case runs SGLang, which
// each VM pulls once from the public sources of the built-in image.
const aliyunSGLangImage = "gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2"

var aliyunSGLangImageSources = []string{
	"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2",
	"quay.io/gpuworks/gpuworks-runner:cuda13.0-sglang0.5.21-r2",
}

// unusedGoldenRunnerTags are golden-image runners this case never starts.
var unusedGoldenRunnerTags = []string{
	"gpuworks/gpuworks-runner:cuda12.8-v100-vllm1cat1.5.1-r1",
	"swr.cn-southwest-2.myhuaweicloud.com/gpuworks/gpuworks-runner:cuda12.8-v100-vllm1cat1.5.1-r1",
}

const aliyunSGLangPullTimeout = 60 * time.Minute

var (
	bootstrapPortFlag = regexp.MustCompile(`--disaggregation-bootstrap-port[ =](\d+)`)
	leaderPortEcho    = regexp.MustCompile(`leader_port=(\d+) own_port=(\d+)`)
)

// TestAliyunSGLangPDMultiNodePrefillTwoVMDocker is the multi-node SGLang
// prefill contract on two Aliyun VMs in one zone (ADR 0028): one prefill
// instance of tensor parallel 2 whose leader rank is on VM1 and whose follower
// rank is on VM2, and a one-GPU decode instance on VM1. Only the prefill
// leader runs the bootstrap server, so the SGLang command the platform
// composes must give the follower the LEADER's side channel port as its
// --disaggregation-bootstrap-port, not the port reserved for the follower
// itself. The case proves the follower's recorded launch and its real
// container command carry the leader's port, that a chat through the gateway
// is answered through PD (SGLang's decoder cannot prefill on its own, so the
// answer needs the two prefill ranks' KV), and that the manual-command
// variable ${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT} expands the same way.
//
//	GPUWORKS_CLOUD_KEEP_ON_FAILURE=1 go test -tags=cloudgpu,aliyun -run '^TestAliyunSGLangPDMultiNodePrefillTwoVMDocker$' -timeout=180m ./test/l4
func TestAliyunSGLangPDMultiNodePrefillTwoVMDocker(t *testing.T) {
	b := aliyunBackend(t)
	var pull *sglangImagePull
	harness := newHostedControlPlane(t, hostedControlPlaneConfig{
		RunID: cloudTestRunID(), RuntimeDrivers: [2]string{"docker", "docker"},
		Backend: b,
		PrepareVMs: func(t *testing.T, envs []*cloudTestEnv) {
			b.EnsureDocker(t, envs)
			requireAliyunGoldenImage(t, envs, b.DistributedImage)
			// The pull runs while the control plane and agents come up.
			pull = startSGLangImagePull(t, envs)
		},
	})
	t.Cleanup(func() { dumpSGLangContainerLogs(t, harness) })
	if harness.gpusPerVM < 2 {
		t.Fatalf("rented %d GPUs per VM, the case needs 2", harness.gpusPerVM)
	}
	assertTwoVMDockerGPUInventories(t, harness.Nodes, harness.gpusPerVM)
	hostedRequireDistributedPreflight(t, harness, 29500)
	model := hostedPrepareRealModel(t, harness)
	pull.wait(t)

	nodeA, nodeB := harness.Nodes[0], harness.Nodes[1]
	body := map[string]any{
		"model_id": model.ID, "served_name": b.servedName("sglang-pd-mn"),
		"engine": "sglang", "image": aliyunSGLangImage,
		"quick_params": map[string]any{"max_model_len": 4096, "enable_metrics": true},
		"placement": map[string]any{"pd": map[string]any{"mode": "sglang-nixl", "roles": map[string]any{
			"prefill": map[string]any{"tp": 2, "units": []any{map[string]any{
				"node_uuid": nodeA.UUID, "gpus": []int{0},
				"followers": []any{map[string]any{"node_uuid": nodeB.UUID, "gpus": []int{0}}},
			}}},
			"decode": map[string]any{"tp": 1, "units": []any{map[string]any{"node_uuid": nodeA.UUID, "gpus": []int{1}}}},
		}}},
	}
	started := time.Now()
	d := createHostedDeployment(t, harness, body, 3, compsharePDDeploymentTimeout)
	t.Logf("TIMING sglang PD deployment create-to-three-running=%s", time.Since(started).Round(time.Second))

	leader, follower, decode := sglangPDInstances(t, d.instances)
	if leader.NodeUUID != nodeA.UUID || follower.NodeUUID != nodeB.UUID || decode.NodeUUID != nodeA.UUID {
		t.Fatalf("PD ranks placed prefill leader=%s follower=%s decode=%s, want %s, %s and %s",
			leader.NodeUUID, follower.NodeUUID, decode.NodeUUID, nodeA.UUID, nodeB.UUID, nodeA.UUID)
	}
	leaderPort := assertBootstrapPortNamesLeader(t, leader, follower)
	t.Logf("prefill leader bootstrap port %d (leader ports %v), follower launched with %d (follower ports %v)",
		leaderPort, leader.Ports, leaderPort, follower.Ports)

	// What the container really runs, read on its own VM, not just what the
	// platform recorded.
	got := dockerCommandBootstrapPort(t, harness.VMs[1], follower)
	if got != leaderPort {
		t.Fatalf("follower container runs --disaggregation-bootstrap-port %d, the leader's is %d", got, leaderPort)
	}
	if own := dockerCommandBootstrapPort(t, harness.VMs[0], leader); own != leaderPort {
		t.Fatalf("leader container runs --disaggregation-bootstrap-port %d, its recorded launch says %d", own, leaderPort)
	}

	assertFollowerRegistered(t, harness, leaderPort)

	// SGLang's decoder cannot prefill in disaggregation mode: an answer means
	// both prefill ranks registered with the leader's bootstrap server and the
	// KV crossed to the decoder.
	d.models(t)
	chatStarted := time.Now()
	reply := d.chat(t)
	t.Logf("TIMING gateway chat through multi-node PD=%s reply=%s", time.Since(chatStarted).Round(time.Millisecond), strings.TrimSpace(string(reply)))
	logPDHandoffEvents(t, harness, leader)
	d.stop(t)

	// The same deployment again from one manual script shared by every rank:
	// ${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT} is the leader's port on the
	// follower and the rank's own on the leader, next to the rank's own port.
	// A decoder has no side channel and states neither variable, so the line
	// runs in the prefill branch only.
	manual := hostedCreateWithManualPrefix(t, harness, body, b.servedName("sglang-pd-mn-manual"),
		"case \"${GPUWORKS_PD_ROLE}\" in\n  prefill) echo \""+manualCommandMarker+
			" leader_port=${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT} own_port=${GPUWORKS_PD_SIDE_CHANNEL_PORT}\" ;;\nesac\n",
		3, compsharePDDeploymentTimeout)
	mLeader, mFollower, _ := sglangPDInstances(t, manual.instances)
	leaderEcho := manualLeaderPortEcho(t, mLeader)
	followerEcho := manualLeaderPortEcho(t, mFollower)
	if leaderEcho[0] != leaderEcho[1] {
		t.Fatalf("leader's ${GPUWORKS_PD_LEADER_SIDE_CHANNEL_PORT}=%d is not its own side channel port %d", leaderEcho[0], leaderEcho[1])
	}
	if followerEcho[0] != leaderEcho[0] || followerEcho[1] == leaderEcho[0] {
		t.Fatalf("follower leader_port=%d own_port=%d, want the leader's %d and a different own port", followerEcho[0], followerEcho[1], leaderEcho[0])
	}
	assertManualCommandRan(t, harness.VMs[0], mLeader, fmt.Sprintf("leader_port=%d own_port=%d", leaderEcho[0], leaderEcho[1]))
	assertManualCommandRan(t, harness.VMs[1], mFollower, fmt.Sprintf("leader_port=%d own_port=%d", followerEcho[0], followerEcho[1]))
	manual.chat(t)
	manual.stop(t)
	t.Logf("TIMING complete SGLang multi-node PD case=%s", time.Since(started).Round(time.Second))
}

// hostedCreateWithManualPrefix deploys body as a manual command: the draft the
// evaluation offers, behind prefix, a shell fragment of its own that every
// rank runs first (hostedCreateWithManualDraft adds one echo line, which
// cannot be limited to the roles that state the variables it names).
func hostedCreateWithManualPrefix(t *testing.T, harness *hostedControlPlane, body map[string]any, servedName, prefix string, expected int, timeout time.Duration) *hostedDeployment {
	t.Helper()
	var evaluated struct {
		ManualCommandDraft string `json:"manual_command_draft"`
	}
	if err := harness.API.PostJSON(t.Context(), "/api/v1/evaluate", body, &evaluated); err != nil {
		t.Fatalf("evaluate for the manual command draft: %v", err)
	}
	rest, ok := strings.CutPrefix(evaluated.ManualCommandDraft, "set -eu\n")
	if !ok {
		t.Fatalf("manual command draft = %q, want a script starting with set -eu", evaluated.ManualCommandDraft)
	}
	manual := maps.Clone(body)
	manual["container_overrides"] = map[string]any{"command_mode": "manual", "cmd_display": "set -eu\n" + prefix + rest}
	manual["served_name"] = servedName
	return createHostedDeployment(t, harness, manual, expected, timeout)
}

// sglangPDInstances splits a multi-node-prefill deployment into the prefill
// leader (rank 0), the prefill follower and the decode instance.
func sglangPDInstances(t *testing.T, instances []dto.Instance) (leader, follower, decode dto.Instance) {
	t.Helper()
	for _, inst := range instances {
		switch {
		case inst.Role == "prefill" && inst.NodeRank == 0:
			leader = inst
		case inst.Role == "prefill" && inst.NodeRank == 1:
			follower = inst
		case inst.Role == "decode":
			decode = inst
		}
	}
	if leader.UUID == "" || follower.UUID == "" || decode.UUID == "" {
		t.Fatalf("PD instances are not one prefill leader, one prefill follower and one decoder: %+v", instances)
	}
	return leader, follower, decode
}

// assertBootstrapPortNamesLeader checks the recorded launches: the leader's
// bootstrap port is one it reserved, and the follower names that same port
// rather than one of its own reservations. It returns the leader's port.
func assertBootstrapPortNamesLeader(t *testing.T, leader, follower dto.Instance) int {
	t.Helper()
	leaderPort := bootstrapPortOf(t, leader)
	followerPort := bootstrapPortOf(t, follower)
	if !slices.Contains(leader.Ports, leaderPort) {
		t.Fatalf("leader bootstrap port %d is not among the ports it reserved %v", leaderPort, leader.Ports)
	}
	if followerPort != leaderPort {
		t.Fatalf("follower --disaggregation-bootstrap-port %d, want the leader's %d (follower reserved %v)", followerPort, leaderPort, follower.Ports)
	}
	if slices.Contains(follower.Ports, followerPort) {
		t.Fatalf("follower bootstrap port %d is one of its own reserved ports %v: the follower got its own port instead of the leader's", followerPort, follower.Ports)
	}
	return leaderPort
}

// launchText is the command the platform recorded for an instance: the
// entrypoint (the whole SGLang command line) followed by its arguments.
func launchText(inst dto.Instance) string {
	return strings.Join(append(slices.Clone(inst.Entrypoint), inst.Argv...), " ")
}

func bootstrapPortOf(t *testing.T, inst dto.Instance) int {
	t.Helper()
	m := bootstrapPortFlag.FindStringSubmatch(launchText(inst))
	if m == nil {
		t.Fatalf("instance %s (role %s rank %d) launch has no --disaggregation-bootstrap-port: %q", inst.UUID, inst.Role, inst.NodeRank, launchText(inst))
	}
	port, _ := strconv.Atoi(m[1])
	return port
}

// dockerCommandBootstrapPort reads the bootstrap port from the arguments the
// instance's container was really started with.
func dockerCommandBootstrapPort(t *testing.T, vm *cloudTestEnv, inst dto.Instance) int {
	t.Helper()
	out, err := runCloudCommand(t.Context(), vm.Instance,
		`docker inspect -f '{{.Path}} {{join .Args " "}}' `+shellQuoteForTest(inst.ContainerID))
	if err != nil {
		t.Fatalf("inspect container %s of %s: %v\n%s", inst.ContainerID, inst.UUID, err, out)
	}
	m := bootstrapPortFlag.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("container %s of %s runs no --disaggregation-bootstrap-port:\n%s", inst.ContainerID, inst.UUID, out)
	}
	port, _ := strconv.Atoi(m[1])
	return port
}

// manualLeaderPortEcho returns {leader port, own port} the manual script of
// the instance printed, from its recorded resolved script.
func manualLeaderPortEcho(t *testing.T, inst dto.Instance) [2]int {
	t.Helper()
	script := strings.Join(append(slices.Clone(inst.Entrypoint), inst.Argv...), "\n")
	m := leaderPortEcho.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("manual script of %s (role %s rank %d) lacks the leader_port echo:\n%s", inst.UUID, inst.Role, inst.NodeRank, script)
	}
	leader, _ := strconv.Atoi(m[1])
	own, _ := strconv.Atoi(m[2])
	return [2]int{leader, own}
}

// assertFollowerRegistered asks the leader's bootstrap server, from the
// follower's VM, for the route of each tensor-parallel rank of the prefill and
// checks that rank 1 registered with the follower VM's address: the server
// only answers for a rank that reached it on the leader's port.
func assertFollowerRegistered(t *testing.T, harness *hostedControlPlane, port int) {
	t.Helper()
	leaderIP, followerIP := harness.VMs[0].Instance.PrivateIP, harness.VMs[1].Instance.PrivateIP
	for rank, wantIP := range []string{leaderIP, followerIP} {
		out, err := runCloudCommand(t.Context(), harness.VMs[1].Instance, fmt.Sprintf(
			"curl -fsS -m 10 'http://%s:%d/route?prefill_dp_rank=0&prefill_cp_rank=0&target_tp_rank=%d&target_pp_rank=0'", leaderIP, port, rank))
		if err != nil {
			t.Fatalf("bootstrap route of prefill rank %d on %s:%d: %v\n%s", rank, leaderIP, port, err, out)
		}
		var route struct {
			IP   string `json:"rank_ip"`
			Port int    `json:"rank_port"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(tailLine(out))), &route); err != nil || route.IP != wantIP || route.Port == 0 {
			t.Fatalf("bootstrap route of prefill rank %d = %q, want an entry at %s", rank, strings.TrimSpace(out), wantIP)
		}
		t.Logf("leader bootstrap server %s:%d routes prefill rank %d to %s:%d", leaderIP, port, rank, route.IP, route.Port)
	}
}

func logPDHandoffEvents(t *testing.T, harness *hostedControlPlane, leader dto.Instance) {
	t.Helper()
	var events struct {
		Items []dto.InstanceEvent `json:"items"`
	}
	if err := harness.API.GetJSON(t.Context(), "/api/v1/instances/"+leader.UUID+"/events", &events); err != nil {
		t.Logf("read prefill leader events: %v", err)
		return
	}
	for _, event := range events.Items {
		t.Logf("prefill leader event %s %v", event.Code, event.Params)
	}
}

// dumpSGLangContainerLogs writes the tail of every gpuworks container's log on
// both VMs when the case failed, so the scene is diagnosable even after the
// harness has stopped the control plane. Containers and VMs stay when
// GPUWORKS_CLOUD_KEEP_ON_FAILURE=1.
func dumpSGLangContainerLogs(t *testing.T, harness *hostedControlPlane) {
	if !t.Failed() || harness == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for rank, vm := range harness.VMs {
		out, err := runCloudCommand(ctx, vm.Instance, `for c in $(docker ps -a --format '{{.Names}}' | grep '^gpuworks-'); do echo "=== $c"; docker logs --tail 120 "$c" 2>&1; done`)
		t.Logf("VM rank %d engine container logs (err %v):\n%s", rank, err, out)
	}
}

// sglangImagePull is one background pull of the SGLang runner per VM.
type sglangImagePull struct {
	envs []*cloudTestEnv
}

// startSGLangImagePull starts, on every VM, a detached pull of the SGLang
// runner from its public sources unless it is already there, so the transfer
// overlaps the control plane's setup.
func startSGLangImagePull(t *testing.T, envs []*cloudTestEnv) *sglangImagePull {
	t.Helper()
	script := goldenimage.PullImageCommand(aliyunSGLangImage, aliyunSGLangImageSources, "")
	encoded := base64.StdEncoding.EncodeToString([]byte(script))
	// The 160 GiB system disk holds the golden image's three vLLM runners
	// (~102 GiB) and leaves 6 GiB while the SGLang runner (~35 GiB) is still
	// being unpacked; the Volta runner is unused here, so it goes first.
	start := fmt.Sprintf(`set -eu
for tag in %[3]s; do docker rmi "$tag" >/dev/null 2>&1 || true; done
if docker image inspect %[1]s >/dev/null 2>&1; then echo 0 >/root/sglang-pull.rc; exit 0; fi
rm -f /root/sglang-pull.rc
printf %%s %[2]s | base64 -d >/root/sglang-pull.sh
setsid sh -c 'sh /root/sglang-pull.sh >/root/sglang-pull.log 2>&1 </dev/null; echo $? >/root/sglang-pull.rc' >/dev/null 2>&1 </dev/null &
echo started`, shellQuoteForTest(aliyunSGLangImage), shellQuoteForTest(encoded), strings.Join(unusedGoldenRunnerTags, " "))
	for rank, env := range envs {
		if out, err := runCloudCommand(t.Context(), env.Instance, start); err != nil {
			t.Fatalf("rank %d start the SGLang image pull: %v\n%s", rank, err, out)
		}
	}
	return &sglangImagePull{envs: envs}
}

// wait blocks until every VM holds the image and logs how long it took.
func (p *sglangImagePull) wait(t *testing.T) {
	t.Helper()
	started := time.Now()
	deadline := started.Add(aliyunSGLangPullTimeout)
	lastLog := started
	for rank, env := range p.envs {
		for {
			out, err := runCloudCommand(t.Context(), env.Instance, `if [ -f /root/sglang-pull.rc ]; then echo "RC=$(cat /root/sglang-pull.rc)"; else echo RC=running; fi; tail -n 2 /root/sglang-pull.log 2>/dev/null || true`)
			status, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
			if err == nil && status == "RC=0" {
				break
			}
			if err == nil && status != "RC=running" && strings.HasPrefix(status, "RC=") {
				t.Fatalf("rank %d SGLang image pull failed (%s):\n%s", rank, status, out)
			}
			if time.Since(lastLog) > 5*time.Minute {
				lastLog = time.Now()
				t.Logf("rank %d SGLang image pull running after %s: %s", rank, time.Since(started).Round(time.Second), strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))
			}
			if time.Now().After(deadline) {
				t.Fatalf("rank %d SGLang image pull did not finish within %s:\n%s", rank, aliyunSGLangPullTimeout, out)
			}
			time.Sleep(20 * time.Second)
		}
		t.Logf("rank %d holds %s after %s (waited from the end of control-plane setup)", rank, aliyunSGLangImage, time.Since(started).Round(time.Second))
	}
}
