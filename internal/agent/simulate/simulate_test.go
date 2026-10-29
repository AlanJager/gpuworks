package simulate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The simulated node must stand for another machine to the agent.
var (
	_ gpu.Prober     = (*Prober)(nil)
	_ runtime.Driver = (*Runtime)(nil)
)

func writeSpec(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAcceptsACompleteSpec(t *testing.T) {
	s, err := Load(writeSpec(t, `{"hostname":"gpu-h100-01","vendor":"nvidia","card":"NVIDIA H100","count":8,"vram_mib":81559,
		"engines":"http://127.0.0.1:1","engines_token":"tok","port_slot":2,"port_slots":6}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Hostname != "gpu-h100-01" || s.Count != 8 || s.VRAMMiB != 81559 || s.PortSlot != 2 || s.PortSlots != 6 || s.EnginesToken != "tok" {
		t.Fatalf("spec = %+v", s)
	}
}

func TestLoadRefusesIncompleteSpecs(t *testing.T) {
	for name, tc := range map[string]struct{ raw, want string }{
		"missing hostname":   {`{"engines":"http://e"}`, "hostname is required"},
		"blank hostname":     {`{"hostname":"  ","engines":"http://e"}`, "hostname is required"},
		"negative count":     {`{"hostname":"n","count":-1,"engines":"http://e"}`, "out of range"},
		"too many cards":     {`{"hostname":"n","count":65,"vendor":"nvidia","card":"c","vram_mib":1,"engines":"http://e"}`, "out of range"},
		"cards without size": {`{"hostname":"n","count":2,"vendor":"nvidia","card":"c","engines":"http://e"}`, "cards need"},
		"slot past slots":    {`{"hostname":"n","port_slot":6,"port_slots":6,"engines":"http://e"}`, "port slot"},
		"negative slot":      {`{"hostname":"n","port_slot":-1,"port_slots":6,"engines":"http://e"}`, "port slot"},
		"missing engines":    {`{"hostname":"n"}`, "engine host is required"},
		"not json":           {`{"hostname":`, "simulated node"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeSpec(t, tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.json")); !os.IsNotExist(err) {
		t.Fatalf("missing file: err = %v", err)
	}
}

// engineHost records the claims and releases a runtime sends, and answers
// with status (0 = 204).
type engineHost struct {
	mu       sync.Mutex
	status   int
	auth     []string
	claims   []map[string]any
	releases []map[string]any
}

func (h *engineHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	h.mu.Lock()
	h.auth = append(h.auth, r.Header.Get("Authorization"))
	switch r.URL.Path {
	case "/claim":
		h.claims = append(h.claims, body)
	case "/release":
		h.releases = append(h.releases, body)
	}
	status := h.status
	h.mu.Unlock()
	if status != 0 {
		http.Error(w, "engine port 30000 is held by another node", status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func newSimulated(t *testing.T, host *engineHost) (*Runtime, *Prober) {
	t.Helper()
	srv := httptest.NewServer(host)
	t.Cleanup(srv.Close)
	return New(Spec{Hostname: "gpu-a100-01", Vendor: "nvidia", Card: "A100", Count: 8, VRAMMiB: 81920, TDP: 400,
		Engines: srv.URL + "/", EnginesToken: "secret", PortSlot: 1, PortSlots: 4})
}

func engineSpec(name string, port int, gpus ...int) runtime.WorkloadSpec {
	return runtime.WorkloadSpec{Name: name, Image: "vllm/vllm-openai", Entrypoint: []string{"vllm", "serve"},
		Cmd: []string{"/models/qwen", "--served-model-name", "qwen3-8b-chat", "--port", "8000"}, Ports: []int{port}, GPUIndexes: gpus}
}

func TestRunClaimsTheEnginePortAndStopReleasesIt(t *testing.T) {
	host := &engineHost{}
	rt, prober := newSimulated(t, host)
	ctx := context.Background()
	id, err := rt.Run(ctx, engineSpec("inst-a", 30001, 0))
	if err != nil {
		t.Fatal(err)
	}
	if len(host.claims) != 1 {
		t.Fatalf("claims = %v", host.claims)
	}
	claim := host.claims[0]
	if claim["node"] != "gpu-a100-01" || claim["container"] != id || claim["port"] != float64(30001) || claim["model"] != "qwen3-8b-chat" {
		t.Fatalf("claim = %v", claim)
	}
	if host.auth[0] != "Bearer secret" {
		t.Fatalf("authorization = %q", host.auth[0])
	}
	if st, _ := rt.Status(ctx, "inst-a"); !st.Exists || !st.Running {
		t.Fatalf("status by name = %+v", st)
	}
	if !prober.MayDial("127.0.0.1:30001") || prober.MayDial("127.0.0.1:30002") {
		t.Fatal("a tunnel must reach the claimed port and only it")
	}
	if names, _ := rt.RunningContainers(ctx); len(names) != 1 || names[0] != "inst-a" {
		t.Fatalf("running = %v", names)
	}

	// Stop by name resolves to the container.
	if err := rt.Stop(ctx, "inst-a"); err != nil {
		t.Fatal(err)
	}
	if len(host.releases) != 1 || host.releases[0]["container"] != id || host.releases[0]["node"] != "gpu-a100-01" {
		t.Fatalf("releases = %v", host.releases)
	}
	if st, _ := rt.Status(ctx, id); !st.Exists || st.Running {
		t.Fatalf("stopped status = %+v", st)
	}
	if prober.MayDial("127.0.0.1:30001") {
		t.Fatal("a stopped instance's port must not stay dialable")
	}
	// A second stop releases nothing more.
	if err := rt.Stop(ctx, id); err != nil || len(host.releases) != 1 {
		t.Fatalf("second stop: err=%v releases=%d", err, len(host.releases))
	}
	if err := rt.Remove(ctx, "inst-a"); err != nil {
		t.Fatal(err)
	}
	if st, _ := rt.Status(ctx, "inst-a"); st.Exists {
		t.Fatalf("removed status = %+v", st)
	}
	if names, _ := rt.ListContainers(ctx); len(names) != 0 {
		t.Fatalf("containers after remove = %v", names)
	}
}

func TestRemoveOfARunningInstanceReleasesItsPort(t *testing.T) {
	host := &engineHost{}
	rt, _ := newSimulated(t, host)
	ctx := context.Background()
	if _, err := rt.Run(ctx, engineSpec("inst-b", 30002, 1)); err != nil {
		t.Fatal(err)
	}
	if err := rt.Remove(ctx, "inst-b"); err != nil {
		t.Fatal(err)
	}
	if len(host.releases) != 1 {
		t.Fatalf("releases = %v", host.releases)
	}
}

func TestRunFailsWhenTheEngineHostRefuses(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusInternalServerError, http.StatusUnauthorized} {
		host := &engineHost{status: status}
		rt, prober := newSimulated(t, host)
		ctx := context.Background()
		_, err := rt.Run(ctx, engineSpec("inst-c", 30003, 2))
		if err == nil || !strings.Contains(err.Error(), "start the simulated engine") || !strings.Contains(err.Error(), "held by another node") {
			t.Fatalf("status %d: err = %v", status, err)
		}
		if st, _ := rt.Status(ctx, "inst-c"); st.Exists {
			t.Fatalf("status %d: a refused instance must not exist: %+v", status, st)
		}
		if prober.MayDial("127.0.0.1:30003") {
			t.Fatalf("status %d: a refused port must not be dialable", status)
		}
		if m, _ := prober.Metrics(ctx); m[2].Values["gpuworks_gpu_memory_used_bytes"] > 0.01*m[2].Values["gpuworks_gpu_memory_total_bytes"] {
			t.Fatalf("status %d: a refused instance must hold no card", status)
		}
	}
}

func TestRunFailsWhenTheEngineHostIsGone(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	rt, _ := New(Spec{Hostname: "n", Engines: srv.URL})
	if _, err := rt.Run(context.Background(), engineSpec("x", 30004)); err == nil {
		t.Fatal("run must fail without an engine host")
	}
}

func TestStopReportsAFailedRelease(t *testing.T) {
	host := &engineHost{}
	rt, _ := newSimulated(t, host)
	ctx := context.Background()
	if _, err := rt.Run(ctx, engineSpec("inst-d", 30005)); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.status = http.StatusInternalServerError
	host.mu.Unlock()
	if err := rt.Stop(ctx, "inst-d"); err == nil {
		t.Fatal("a failed release must be reported")
	}
	// The claim is kept until the engine host lets it go: removing retries
	// the release, and fails while the host still refuses.
	if err := rt.Remove(ctx, "inst-d"); err == nil {
		t.Fatal("a remove whose release failed must be reported")
	}
	host.mu.Lock()
	host.status = 0
	host.mu.Unlock()
	if err := rt.Remove(ctx, "inst-d"); err != nil {
		t.Fatalf("remove once the host answers: %v", err)
	}
	if err := rt.Remove(ctx, "inst-d"); err != nil {
		t.Fatalf("a removed instance removes again: %v", err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.releases) != 3 {
		t.Fatalf("releases = %v, want the two failed and the one that held", host.releases)
	}
}

func TestReleaseAllLetsGoOfTheNodesEngines(t *testing.T) {
	host := &engineHost{}
	rt, _ := newSimulated(t, host)
	if err := rt.ReleaseAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.releases) != 1 || host.releases[0]["node"] != "gpu-a100-01" || host.releases[0]["container"] != nil {
		t.Fatalf("releases = %v", host.releases)
	}
}

func TestPartitionProbeExitsWithoutAClaim(t *testing.T) {
	host := &engineHost{}
	rt, _ := newSimulated(t, host)
	ctx := context.Background()
	id, err := rt.Run(ctx, runtime.WorkloadSpec{Name: "probe", Image: assets.PartitionProbeImage, Ports: []int{30006}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := rt.Status(ctx, id)
	if !st.Exists || st.Running || len(host.claims) != 0 {
		t.Fatalf("probe status = %+v, claims = %v", st, host.claims)
	}
}

func TestHeadlessAndPortlessRanksRunWithoutAClaim(t *testing.T) {
	host := &engineHost{}
	rt, prober := newSimulated(t, host)
	ctx := context.Background()
	headless := engineSpec("follower", 30007, 3)
	headless.Cmd = append(headless.Cmd, "--headless")
	if _, err := rt.Run(ctx, headless); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Run(ctx, runtime.WorkloadSpec{Name: "side", GPUIndexes: []int{4}}); err != nil {
		t.Fatal(err)
	}
	if len(host.claims) != 0 {
		t.Fatalf("claims = %v", host.claims)
	}
	for _, name := range []string{"follower", "side"} {
		if st, _ := rt.Status(ctx, name); !st.Running {
			t.Fatalf("%s status = %+v", name, st)
		}
	}
	if prober.MayDial("127.0.0.1:30007") {
		t.Fatal("a headless rank's side port is not an engine")
	}
	if err := rt.Stop(ctx, "follower"); err != nil || len(host.releases) != 0 {
		t.Fatalf("stop: err=%v releases=%v", err, host.releases)
	}
}

func TestServedModelFallsBackToTheWorkloadName(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"--served-model-name", "a"}, "a"},
		{[]string{"--served-model-name=b"}, "b"},
		{[]string{"--served-model-name"}, "fallback"},
		{nil, "fallback"},
	} {
		if got := servedModel(tc.argv, "fallback"); got != tc.want {
			t.Fatalf("servedModel(%v) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestDGPUSliceHoldsItsShareOfTheCard(t *testing.T) {
	host := &engineHost{}
	rt, prober := newSimulated(t, host)
	ctx := context.Background()
	slice := engineSpec("slice", 30008, 0)
	slice.Env = map[string]string{accelerator.DGPUEnvMemoryLimitPrefix + "0": "21474836480"} // 20 GiB of 80
	if _, err := rt.Run(ctx, slice); err != nil {
		t.Fatal(err)
	}
	if got := prober.cards.share(0); got < 0.24 || got > 0.26 {
		t.Fatalf("share = %v, want 0.25", got)
	}
	for _, bad := range []string{"", "not-a-number", "-5"} {
		s := engineSpec("whole-"+bad, 30009, 1)
		s.Env = map[string]string{accelerator.DGPUEnvMemoryLimitPrefix + "0": bad}
		if got := rt.share(s); got != 1 {
			t.Fatalf("share for limit %q = %v, want the whole card", bad, got)
		}
	}
	big := engineSpec("big", 30010, 1)
	big.Env = map[string]string{accelerator.DGPUEnvMemoryLimitPrefix + "0": "999999999999999"}
	if got := rt.share(big); got != 1 {
		t.Fatalf("a limit past the card = %v, want 1", got)
	}
	m, _ := prober.Metrics(ctx)
	used, total := m[0].Values["gpuworks_gpu_memory_used_bytes"], m[0].Values["gpuworks_gpu_memory_total_bytes"]
	if used < 0.2*total || used > 0.3*total {
		t.Fatalf("slice memory = %.0f of %.0f, want about a quarter", used, total)
	}
}

func TestExecIsRefused(t *testing.T) {
	rt, _ := New(Spec{Hostname: "n", Engines: "http://e"})
	if _, err := rt.Exec(context.Background(), "x", []string{"sh"}); err == nil {
		t.Fatal("exec must be refused on a simulated node")
	}
}

func TestProbeListsTheSpecsCards(t *testing.T) {
	_, p := New(Spec{Hostname: "ppu-01", Vendor: "ppu", Card: "PPU-ZW810E", Count: 3, VRAMMiB: 98304, ArchID: "Beethoven", Engines: "http://e"})
	cards, err := p.Probe(context.Background())
	if err != nil || len(cards) != 3 {
		t.Fatalf("cards = %v, err = %v", cards, err)
	}
	for i, c := range cards {
		if c.Index != i || c.Name != "PPU-ZW810E" || c.MemoryTotalBytes != 98304<<20 || c.Vendor != "ppu" || c.ArchID != "Beethoven" || !c.Healthy || c.UUID != "GPU-SIM-ppu-01-"+string(rune('0'+i)) {
			t.Fatalf("card %d = %+v", i, c)
		}
	}
	if p.Host().LibvgpuExtractor() != nil {
		t.Fatal("only simulated NVIDIA cards offer dGPU without the library")
	}
	_, nv := New(Spec{Hostname: "n", Vendor: "nvidia", Card: "c", Count: 1, VRAMMiB: 1, ComputeCapability: "8.0", Engines: "http://e"})
	if c, _ := nv.Probe(context.Background()); c[0].ArchID != "8.0" || nv.Host().LibvgpuExtractor() == nil {
		t.Fatalf("nvidia card = %+v", c[0])
	}
	_, none := New(Spec{Hostname: "cpu", Engines: "http://e"})
	if none.Available() {
		t.Fatal("a node without cards has no accelerator")
	}
}

func TestMetricsFollowWhatTheInstancesHold(t *testing.T) {
	host := &engineHost{}
	rt, prober := newSimulated(t, host)
	ctx := context.Background()
	if _, err := rt.Run(ctx, engineSpec("busy", 30011, 0, 1)); err != nil {
		t.Fatal(err)
	}
	// The random walk settles within a few samples.
	var m []gpu.Metric
	for i := 0; i < 30; i++ {
		m, _ = prober.Metrics(ctx)
	}
	if len(m) != 8 {
		t.Fatalf("metrics for %d cards", len(m))
	}
	for _, busy := range []int{0, 1} {
		for _, idle := range []int{2, 7} {
			b, i := m[busy].Values, m[idle].Values
			if b["gpuworks_gpu_utilization"] <= i["gpuworks_gpu_utilization"]+10 {
				t.Fatalf("card %d util %v not above idle card %d %v", busy, b["gpuworks_gpu_utilization"], idle, i["gpuworks_gpu_utilization"])
			}
			if b["gpuworks_gpu_memory_used_bytes"] < 0.8*b["gpuworks_gpu_memory_total_bytes"] || i["gpuworks_gpu_memory_used_bytes"] > 0.01*i["gpuworks_gpu_memory_total_bytes"] {
				t.Fatalf("memory: busy %v idle %v", b["gpuworks_gpu_memory_used_bytes"], i["gpuworks_gpu_memory_used_bytes"])
			}
			if b["gpuworks_gpu_power_watts"] <= i["gpuworks_gpu_power_watts"] {
				t.Fatalf("power: busy %v idle %v", b["gpuworks_gpu_power_watts"], i["gpuworks_gpu_power_watts"])
			}
		}
	}
	for _, card := range m {
		if u := card.Values["gpuworks_gpu_utilization"]; u < 0 || u > 100 {
			t.Fatalf("utilization %v out of range", u)
		}
	}
	if err := rt.Stop(ctx, "busy"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		m, _ = prober.Metrics(ctx)
	}
	if m[0].Values["gpuworks_gpu_memory_used_bytes"] > 0.01*m[0].Values["gpuworks_gpu_memory_total_bytes"] {
		t.Fatal("a released card must free its memory")
	}
}

func TestSharedCardSumsItsSlicesUpToTheWholeCard(t *testing.T) {
	c := newCards()
	c.hold("a", []int{0}, 0.6)
	c.hold("b", []int{0}, 0.6)
	c.hold("a", []int{0}, 0.6) // holding again changes nothing
	if got := c.share(0); got != 1 {
		t.Fatalf("share = %v, want capped at 1", got)
	}
	c.release("a")
	if got := c.share(0); got < 0.59 || got > 0.61 {
		t.Fatalf("share after release = %v", got)
	}
	c.release("b")
	c.release("never-held")
	if got := c.share(0); got != 0 {
		t.Fatalf("share after both released = %v", got)
	}
}

func TestDescribeHostReportsTheSimulatedMachine(t *testing.T) {
	_, p := New(Spec{Hostname: "npu-910b-01", OS: "linux", Arch: "arm64", Kernel: "5.15.0", CPU: "Kunpeng-920", Cores: 192, MemGiB: 1536, Engines: "http://e"})
	real := proto.SysInfo{Hostname: "my-mac.local", OS: "darwin", Arch: "amd64", Kernel: "Darwin 24", CPUModel: "Apple M3", CPUCores: 8, MemTotalKB: 16 << 20}
	sys := p.DescribeHost(real)
	if sys.Hostname != "npu-910b-01" || sys.OS != "linux" || sys.Arch != "arm64" || sys.Kernel != "5.15.0" || sys.CPUModel != "Kunpeng-920" || sys.CPUCores != 192 {
		t.Fatalf("host = %+v", sys)
	}
	if sys.MemTotalKB != 1536<<20 || sys.MemAvailKB <= 0 || sys.MemAvailKB >= sys.MemTotalKB || sys.DiskAvailKB >= sys.DiskTotalKB {
		t.Fatalf("memory/disk = %+v", sys)
	}
	if h := p.Host(); h.SysInfo(real).Hostname != "npu-910b-01" || !h.Simulated() {
		t.Fatal("the agent must see the simulated host")
	}
	if _, ok := p.Host().HostSamples("n", 0); !ok {
		t.Fatal("the agent must sample the simulated host")
	}
}

func TestMayDialOnlyClaimedLoopbackPorts(t *testing.T) {
	_, p := New(Spec{Hostname: "n", Engines: "http://e"})
	p.cards.claim("sim-1", 30020)
	for addr, want := range map[string]bool{
		"127.0.0.1:30020": true,
		"localhost:30020": false, // may resolve to ::1, which the engine host does not hold
		"127.0.0.1:30021": false,
		"10.0.0.5:30020":  false,
		"0.0.0.0:30020":   false,
		"127.0.0.1:22":    false,
		"127.0.0.1":       false,
		"127.0.0.1:x":     false,
	} {
		if got := p.MayDial(addr); got != want {
			t.Errorf("MayDial(%q) = %v, want %v", addr, got, want)
		}
	}
	h := p.Host()
	if h.Stream(proto.StreamHeader{Kind: proto.StreamTunnel, Dst: "127.0.0.1:30020"}) != nil ||
		h.Stream(proto.StreamHeader{Kind: proto.StreamTunnel, Dst: "127.0.0.1:8800"}) == nil {
		t.Fatal("the node's tunnels must defer to the simulated prober")
	}
}

func TestEnginePortListenersLeaveOnlyTheNodesSlice(t *testing.T) {
	r := proto.PortRange{Start: 20000, End: 20599}
	_, p := New(Spec{Hostname: "n", Engines: "http://e", PortSlot: 2, PortSlots: 6})
	taken := p.EnginePortListeners(r)
	if len(taken) != 500 {
		t.Fatalf("taken %d ports, want 500", len(taken))
	}
	takenSet := map[int]bool{}
	for _, port := range taken {
		if !r.Contains(port) {
			t.Fatalf("port %d outside the range", port)
		}
		takenSet[port] = true
	}
	for port := 20200; port < 20300; port++ {
		if takenSet[port] {
			t.Fatalf("own port %d reported taken", port)
		}
	}
	if !takenSet[20199] || !takenSet[20300] || !takenSet[20000] || !takenSet[20599] {
		t.Fatal("the other slices must be reported taken")
	}
	_, single := New(Spec{Hostname: "n", Engines: "http://e"})
	if got := single.EnginePortListeners(r); got != nil {
		t.Fatalf("a lone node takes the whole range, got %d taken", len(got))
	}
}

func TestHostSamplesNameOnlyTheSimulatedDevices(t *testing.T) {
	host := &engineHost{}
	rt, p := newSimulated(t, host)
	now := time.Date(2026, 10, 28, 10, 30, 0, 0, time.Local).UnixMilli()
	idle := p.HostSamples("node-uuid", now)
	if _, err := rt.Run(context.Background(), engineSpec("busy", 30030, 0, 1, 2, 3, 4, 5, 6, 7)); err != nil {
		t.Fatal(err)
	}
	busy := p.HostSamples("node-uuid", now)
	if len(idle) != 7 || len(busy) != 7 {
		t.Fatalf("samples: idle %d busy %d", len(idle), len(busy))
	}
	realHost, _ := os.Hostname()
	byMetric := map[string]float64{}
	for i, s := range busy {
		if s.Labels["node"] != "node-uuid" || s.TimeMS != now || s.Value < 0 {
			t.Fatalf("sample %+v", s)
		}
		if d, ok := s.Labels["device"]; ok && d != "nvme0n1" {
			t.Fatalf("device label %q", d)
		}
		if d, ok := s.Labels["iface"]; ok && d != "eth0" {
			t.Fatalf("iface label %q", d)
		}
		for _, v := range s.Labels {
			if realHost != "" && v == realHost {
				t.Fatalf("real hostname leaked in %+v", s)
			}
		}
		byMetric[s.Metric] = s.Value
		_ = i
	}
	idleCPU := idle[0].Value
	if byMetric["gpuworks_node_cpu_busy_percent"] <= idleCPU {
		t.Fatalf("cpu busy %v not above idle %v", byMetric["gpuworks_node_cpu_busy_percent"], idleCPU)
	}
	if byMetric["gpuworks_node_net_rx_bytes_per_sec"] != byMetric["gpuworks_node_nic_rx_bytes_per_sec"] {
		t.Fatal("the node's traffic is its one NIC's")
	}
}

func TestRhythmStaysWithinItsBounds(t *testing.T) {
	start := time.Date(2026, 10, 26, 0, 0, 0, 0, time.Local) // a Monday
	for h := 0; h < 7*24*4; h++ {
		at := start.Add(time.Duration(h) * 15 * time.Minute)
		r := Rhythm(at)
		if r < 0.28*0.6-1e-9 || r > 1+1e-9 {
			t.Fatalf("Rhythm(%v) = %v", at, r)
		}
		if u := BusyUtilization(r); u < 28 || u > 94 {
			t.Fatalf("BusyUtilization(%v) = %v", r, u)
		}
	}
	weekdayPeak := Rhythm(time.Date(2026, 10, 28, 10, 30, 0, 0, time.Local))
	weekdayNight := Rhythm(time.Date(2026, 10, 28, 3, 0, 0, 0, time.Local))
	saturdayPeak := Rhythm(time.Date(2026, 10, 31, 10, 30, 0, 0, time.Local))
	if weekdayPeak < 0.9 || weekdayNight > 0.35 || weekdayNight < 0.28 {
		t.Fatalf("weekday peak %v night %v", weekdayPeak, weekdayNight)
	}
	if saturdayPeak >= weekdayPeak || saturdayPeak > 0.61 {
		t.Fatalf("weekend %v must be lighter than weekday %v", saturdayPeak, weekdayPeak)
	}
	lunch := Rhythm(time.Date(2026, 10, 28, 12, 30, 0, 0, time.Local))
	if lunch >= weekdayPeak {
		t.Fatalf("lunch %v not below the morning peak %v", lunch, weekdayPeak)
	}
}

func TestLogsReadLikeAnEngineAndEndWithTheContext(t *testing.T) {
	rt, _ := New(Spec{Hostname: "n", Engines: "http://e"})
	ctx, cancel := context.WithCancel(context.Background())
	rc, err := rt.Logs(ctx, "x", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, _ := rc.Read(buf)
	if !strings.Contains(string(buf[:n]), "INFO") {
		t.Fatalf("first log line = %q", buf[:n])
	}
	cancel()
	done := make(chan struct{})
	go func() {
		for {
			if _, err := rc.Read(buf); err != nil {
				close(done)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("followed logs must end with the context")
	}
}
