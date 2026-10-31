package app

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// nvlinkProber reports the links a test sets, read at the time it sets.
type nvlinkProber struct {
	gpu.FixedProber
	active    int
	at        time.Time
	refreshed int
}

func (p *nvlinkProber) NVLinks(context.Context) (map[string]gpu.NVLinkCard, time.Time, bool) {
	return map[string]gpu.NVLinkCard{"GPU-a": {Active: p.active, Total: 18, CRC: 3}}, p.at, true
}
func (p *nvlinkProber) RefreshTopology() { p.refreshed++ }

func TestADownNVLinkIsRaisedAfterTwoReadingsAndRefreshesTheTopology(t *testing.T) {
	p := &nvlinkProber{active: 18, at: time.Unix(1000, 0)}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none", p, nil)
	read := func(active int, fresh bool) proto.GPUInfo {
		p.active = active
		if fresh {
			p.at = p.at.Add(5 * time.Minute)
		}
		cards := []proto.GPUInfo{{Index: 0, UUID: "GPU-a", Vendor: "nvidia", PCIeBDF: "0000:10:00.0", Healthy: true, HealthStatus: accelerator.HealthOK}}
		n.applyNVLinkFindings(context.Background(), cards)
		return cards[0]
	}
	read(18, false) // the most this card has had
	if c := read(17, true); c.HealthCode != "" {
		t.Fatalf("one low reading raised %q", c.HealthCode)
	}
	// A report between readings reuses the last one and counts nothing.
	if c := read(17, false); c.HealthCode != "" {
		t.Fatalf("a repeated reading counted: %q", c.HealthCode)
	}
	c := read(17, true)
	if c.HealthCode != accelerator.HealthNVLinkDown || c.HealthStatus != accelerator.HealthWarning || !c.Healthy ||
		c.HealthMessage != "NVLink active links=17 of 18\nNVLink errors=replay 0, recovery 0, CRC 3" || p.refreshed != 1 {
		t.Fatalf("after two low readings = %+v refreshed=%d", c, p.refreshed)
	}
	if c := read(18, true); c.HealthCode != accelerator.HealthNVLinkDown {
		t.Fatal("one good reading cleared it")
	}
	if c := read(18, true); c.HealthCode != "" || p.refreshed != 2 {
		t.Fatalf("after two good readings = %+v refreshed=%d", c, p.refreshed)
	}
}
