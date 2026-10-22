package gpu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
