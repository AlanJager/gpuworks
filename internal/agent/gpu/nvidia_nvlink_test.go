package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readGPUFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An HGX H20 (172.30.8.111, driver 580.126.09): eight cards, 18 links each.
func TestParseNVLinksOnAnH20(t *testing.T) {
	cards := parseNVLinks(readGPUFixture(t, "nvidia-smi-nvlink-se-h20.txt"))
	if len(cards) != 8 {
		t.Fatalf("cards = %d", len(cards))
	}
	c, ok := cards["GPU-c8fe5aba-cdca-b86f-aeb1-e8b8f93e45ed"]
	if !ok || c.Active != 18 || c.Total != 18 || c.Replay != 0 || c.CRC != 0 {
		t.Fatalf("GPU 0 = %+v ok=%v", c, ok)
	}
	// A link reported inactive, and errors on another.
	out := strings.Replace(readGPUFixture(t, "nvidia-smi-nvlink-se-h20.txt"), "Link 3: 26.562 GB/s", "Link 3: <inactive>", 1)
	out = strings.Replace(out, "Link 5: CRC Errors: 0", "Link 5: CRC Errors: 12", 1)
	c = parseNVLinks(out)["GPU-c8fe5aba-cdca-b86f-aeb1-e8b8f93e45ed"]
	if c.Active != 17 || c.Total != 18 || c.CRC != 12 {
		t.Fatalf("GPU 0 with a link down = %+v", c)
	}
}

// PCIe-only cards (gpu3): NVML says all links are inactive; the host has no
// NVLink and is asked again only hourly.
func TestNVLinksAbsentHostIsAskedHourly(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	calls := 0
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "nvlink -s -e" {
			return "", errors.New("unexpected " + strings.Join(args, " "))
		}
		calls++
		return readGPUFixture(t, "nvidia-smi-nvlink-se-inactive.txt"), errors.New("exit status 255")
	}, now: func() time.Time { return now }}
	cards, _, ok := p.NVLinks(t.Context())
	if ok || len(cards) != 2 || calls != 1 {
		t.Fatalf("NVLinks = %+v ok=%v calls=%d", cards, ok, calls)
	}
	now = now.Add(30 * time.Minute)
	p.NVLinks(t.Context())
	if calls != 1 {
		t.Fatalf("asked again after %d calls within the hour", calls)
	}
	now = now.Add(31 * time.Minute)
	p.NVLinks(t.Context())
	if calls != 2 {
		t.Fatalf("calls = %d, want the hourly recheck", calls)
	}
}

// A host with NVLink is asked once every five minutes, and a topology
// refresh makes the next probe ask for the topology again.
func TestNVLinksCadenceAndTopologyRefresh(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	calls := 0
	p := &SMIProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls++
		return readGPUFixture(t, "nvidia-smi-nvlink-se-h20.txt"), nil
	}, now: func() time.Time { return now }}
	if _, at, ok := p.NVLinks(t.Context()); !ok || !at.Equal(now) {
		t.Fatalf("first read ok=%v at=%v", ok, at)
	}
	now = now.Add(4 * time.Minute)
	if _, at, _ := p.NVLinks(t.Context()); calls != 1 || at.Equal(now) {
		t.Fatalf("calls = %d at=%v within five minutes", calls, at)
	}
	now = now.Add(2 * time.Minute)
	p.NVLinks(t.Context())
	if calls != 2 {
		t.Fatalf("calls = %d after five minutes", calls)
	}
	p.layout.topology = layoutAnswer{out: "x", at: now}
	p.RefreshTopology()
	if !p.layout.topology.at.IsZero() {
		t.Fatal("topology not marked for refresh")
	}
}
