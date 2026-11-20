package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// layoutHost is an nvidia-smi whose cards, MIG listing and driver a test
// changes between calls; it counts every call by its first argument.
type layoutHost struct {
	inventory string
	listing   string
	driver    string
	calls     map[string]int
}

func newLayoutHost() *layoutHost {
	return &layoutHost{
		inventory: "0, GPU-a, NVIDIA A100, 81920, 8.0, 00000000:65:00.0, 550.90\n",
		listing:   "GPU 0: NVIDIA A100 (UUID: GPU-a)\n  MIG 3g.40gb     Device  0: (UUID: MIG-a)\n",
		driver:    "550.90",
		calls:     map[string]int{},
	}
}

func (h *layoutHost) run(_ context.Context, args ...string) (string, error) {
	key := "smi"
	if len(args) > 0 {
		key = args[0]
	}
	switch {
	case len(args) == 0:
		h.calls[key]++
		return "Driver Version: " + h.driver + "\nCUDA Version: 12.8\n", nil
	case strings.HasPrefix(args[0], "--query-gpu=index,uuid,name"):
		h.calls["inventory"]++
		return h.inventory, nil
	case args[0] == "-L":
		h.calls[key]++
		return h.listing, nil
	case args[0] == "topo":
		h.calls[key]++
		return "topology of " + h.inventory, nil
	}
	return "", errors.New("unexpected call: " + strings.Join(args, " "))
}

// The topology and the MIG listing are asked once for a set of cards: a
// status report every minute reuses them until the cards change, or until
// nvidiaLayoutRecheck has passed. `topo -m` took 11-31 s on gpu3, and asked
// every minute it held up every other query.
func TestNVIDIALayoutIsAskedOncePerSetOfCards(t *testing.T) {
	h := newLayoutHost()
	clock := time.Now()
	p := &SMIProber{hostRoot: t.TempDir(), now: func() time.Time { return clock }, runner: h.run}
	status := func() string {
		t.Helper()
		gpus, err := p.Probe(t.Context())
		if err != nil || len(gpus) != 1 {
			t.Fatalf("Probe = %+v, %v", gpus, err)
		}
		if !gpus[0].MIGEnabled || gpus[0].MemoryTotalBytes != 40<<30 {
			t.Fatalf("card = %+v, want the cached MIG slice applied", gpus[0])
		}
		return p.Topology(t.Context())
	}
	for i := 0; i < 5; i++ {
		if topo := status(); !strings.Contains(topo, "GPU-a") {
			t.Fatalf("topology = %q", topo)
		}
		clock = clock.Add(time.Minute)
	}
	if h.calls["inventory"] != 5 || h.calls["topo"] != 1 || h.calls["-L"] != 1 {
		t.Fatalf("calls = %v, want the inventory every report and the layout once", h.calls)
	}

	// Another card in the same slot is another layout.
	h.inventory = strings.Replace(h.inventory, "GPU-a", "GPU-b", 1)
	if topo := status(); !strings.Contains(topo, "GPU-b") {
		t.Fatalf("topology after the card changed = %q", topo)
	}
	if h.calls["topo"] != 2 || h.calls["-L"] != 2 {
		t.Fatalf("calls = %v, want the layout asked again for the new card", h.calls)
	}

	clock = clock.Add(nvidiaLayoutRecheck)
	status()
	if h.calls["topo"] != 3 || h.calls["-L"] != 3 {
		t.Fatalf("calls = %v, want the layout asked again after %s", h.calls, nvidiaLayoutRecheck)
	}
}

// A layout query that fails is not cached: the next report asks again, and
// meanwhile the server keeps the topology it has.
func TestNVIDIALayoutFailureIsNotCached(t *testing.T) {
	failing := true
	calls := 0
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls++
		if failing {
			return "", errors.New("nvidia-smi: topo failed")
		}
		return "topology", nil
	}}
	if got := p.Topology(t.Context()); got != "" {
		t.Fatalf("Topology after a failure = %q, want empty", got)
	}
	failing = false
	if got := p.Topology(t.Context()); got != "topology" || calls != 2 {
		t.Fatalf("Topology = %q after %d calls, want asked again", got, calls)
	}
}

// The CUDA version belongs to the driver: it is asked once per driver
// version, however long the agent runs, and again when the inventory
// reports another driver.
func TestNVIDIAFrameworkIsAskedOncePerDriver(t *testing.T) {
	h := newLayoutHost()
	p := &SMIProber{hostRoot: t.TempDir(), frameworkTTL: time.Nanosecond, runner: h.run}
	for i := 0; i < 3; i++ {
		if _, err := p.Probe(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, version := p.Framework(t.Context()); version != "12.8" {
			t.Fatalf("Framework version = %q", version)
		}
		time.Sleep(time.Millisecond) // past frameworkTTL, which no longer applies
	}
	if h.calls["smi"] != 1 {
		t.Fatalf("framework queries = %d, want 1 for one driver", h.calls["smi"])
	}
	h.inventory = strings.Replace(h.inventory, "550.90", "580.126.09", 1)
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.Framework(t.Context())
	if h.calls["smi"] != 2 {
		t.Fatalf("framework queries = %d, want asked again for the new driver", h.calls["smi"])
	}
}

// An empty roster is a driver hiccup as often as a real card removal: the
// unified-memory judgment survives it (with no cards listed there are no
// metric rows to fill anyway), and the next roster that lists cards replaces
// it wholesale.
func TestUnifiedJudgmentsSurviveAnEmptyRoster(t *testing.T) {
	p := &SMIProber{}
	p.rememberInventory([]proto.GPUInfo{{Index: 0, UUID: "GPU-thor", Name: "NVIDIA Thor", PCIeBDF: "0000:05:00.0"}})
	if !p.unifiedMemoryCards()["GPU-thor"] {
		t.Fatal("the Thor card was not remembered as unified memory")
	}
	p.rememberInventory(nil)
	if !p.unifiedMemoryCards()["GPU-thor"] {
		t.Fatal("an empty roster wiped the unified-memory judgment")
	}
	p.rememberInventory([]proto.GPUInfo{{Index: 0, UUID: "GPU-5090", Name: "NVIDIA GeForce RTX 5090", PCIeBDF: "0000:05:00.0"}})
	if cards := p.unifiedMemoryCards(); len(cards) != 0 {
		t.Fatalf("a roster of discrete cards left %v behind", cards)
	}
}

// The failure the retention guards against reaches rememberInventory through
// Probe: nvidia-smi exits 0 with no rows, and the next Metrics must still
// backfill the Thor row from the host pool using the retained judgment.
func TestMetricsBackfillSurvivesAnEmptyRosterProbe(t *testing.T) {
	const (
		uuid  = "GPU-e78d5a9c-a30c-482a-a004-123456789abc"
		thor  = "NVIDIA Thor"
		total = 127598832 * 1024
		used  = (127598832 - 100000000) * 1024
	)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "proc/meminfo"), []byte("MemTotal:       127598832 kB\nMemAvailable:   100000000 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const format = "--format=csv,noheader,nounits"
	inventories := []string{
		fmt.Sprintf("0, %s, %s, [N/A], 12.1, 0000000F:01:00.0, 595.78, [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]\n", uuid, thor),
		"", // the hiccup: nvidia-smi exits 0 with no rows
	}
	calls := 0
	p := &SMIProber{hostRoot: root, now: func() time.Time { return time.Now() }, runner: func(_ context.Context, args ...string) (string, error) {
		switch joined := strings.Join(args, " "); {
		case joined == nvidiaInventoryQueries[0]+" "+format:
			calls++
			if calls-1 < len(inventories) {
				return inventories[calls-1], nil
			}
			return "", nil
		case joined == metricsQueryEventReasons+" "+format:
			return fmt.Sprintf("0, %s, [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]\n", uuid), nil
		case joined == "-L":
			return fmt.Sprintf("GPU 0: %s (UUID: %s)\n", thor, uuid), nil
		case joined == "dmon -c 1 -s t":
			return "", fmt.Errorf("dmon is not supported on this platform")
		default:
			t.Errorf("unexpected nvidia-smi command: %q", joined)
			return "", fmt.Errorf("unsupported nvidia-smi command: %q", joined)
		}
	}}
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !p.unifiedMemoryCards()[uuid] {
		t.Fatal("the first probe did not record the Thor judgment")
	}
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !p.unifiedMemoryCards()[uuid] {
		t.Fatal("the empty roster wiped the unified-memory judgment")
	}
	metrics, err := p.Metrics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics) != 1 || metrics[0].UUID != uuid {
		t.Fatalf("metrics = %+v, want the Thor row", metrics)
	}
	if !metrics[0].HostPoolMemory {
		t.Fatal("the backfilled row is not marked HostPoolMemory")
	}
	if got := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; got != used {
		t.Fatalf("used = %v, want the host-pool backfill %v", got, used)
	}
	if got := metrics[0].Values["gpuworks_gpu_memory_total_bytes"]; got != total {
		t.Fatalf("total = %v, want %v", got, total)
	}
}
