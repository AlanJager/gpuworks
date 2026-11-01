package gpu

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// NVLink state, asked of nvidia-smi once every nvlinkInterval: one command
// (`nvlink -s -e`) prints each link's speed and its replay, recovery and CRC
// error counters. A host whose cards have no active link at all (PCIe-only
// cards, as on gpu3) is asked again only every nvlinkAbsentRecheck.
const (
	nvlinkInterval      = accelerator.NVLinkCheckPeriod
	nvlinkAbsentRecheck = time.Hour
)

// NVLinkCard is one card's links.
type NVLinkCard struct {
	// Active links are the ones with a speed; Total counts inactive ones too.
	Active, Total int
	// Error counters since the driver loaded, summed over the links.
	Replay, Recovery, CRC uint64
}

// NVLinkReporter is implemented by probers that can say how each card's
// NVLinks are (NVIDIA).
type NVLinkReporter interface {
	// NVLinks are the cards' links by UUID, and when they were read. ok is
	// false when nothing is known (no NVLink on the host, or not asked yet).
	NVLinks(ctx context.Context) (cards map[string]NVLinkCard, at time.Time, ok bool)
	// RefreshTopology has the next probe ask for the topology again rather
	// than reuse the hour-old answer: a link that went down changes which
	// cards the scheduler puts together.
	RefreshTopology()
}

type nvlinkState struct {
	mu     sync.Mutex
	at     time.Time
	cards  map[string]NVLinkCard
	absent bool
}

// NVLinks implements NVLinkReporter.
func (p *SMIProber) NVLinks(ctx context.Context) (map[string]NVLinkCard, time.Time, bool) {
	p.nvlink.mu.Lock()
	at, cards, absent := p.nvlink.at, p.nvlink.cards, p.nvlink.absent
	p.nvlink.mu.Unlock()
	interval := nvlinkInterval
	if absent {
		interval = nvlinkAbsentRecheck
	}
	if !at.IsZero() && p.clockNow().Sub(at) < interval || p.slow() {
		return cards, at, cards != nil && !absent
	}
	out, err := p.query(ctx, "nvlink", "-s", "-e")
	if err != nil && !strings.Contains(out, "inActive") {
		return cards, at, cards != nil && !absent
	}
	parsed := parseNVLinks(out)
	absent = true
	for _, c := range parsed {
		absent = absent && c.Active == 0
	}
	now := p.clockNow()
	p.nvlink.mu.Lock()
	p.nvlink.at, p.nvlink.cards, p.nvlink.absent = now, parsed, absent
	p.nvlink.mu.Unlock()
	return parsed, now, !absent
}

// RefreshTopology implements NVLinkReporter.
func (p *SMIProber) RefreshTopology() {
	p.layout.mu.Lock()
	p.layout.topology.at = time.Time{}
	p.layout.mu.Unlock()
}

var (
	nvlinkGPURE   = regexp.MustCompile(`^GPU \d+: .*\(UUID: (GPU-[0-9a-fA-F-]+)\)`)
	nvlinkSpeedRE = regexp.MustCompile(`^Link \d+: ([0-9.]+) GB/s`)
	nvlinkErrRE   = regexp.MustCompile(`^Link \d+: (Replay|Recovery|CRC) Errors: (\d+)`)
)

// parseNVLinks reads `nvidia-smi nvlink -s -e`: per card a header line, one
// line per link with its speed or "<inactive>", then three error counters
// per link. A card with no active link at all prints NVML's "all links are
// inActive" instead, and has no links here.
func parseNVLinks(out string) map[string]NVLinkCard {
	cards := map[string]NVLinkCard{}
	uuid := ""
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if m := nvlinkGPURE.FindStringSubmatch(line); m != nil {
			uuid = m[1]
			cards[uuid] = NVLinkCard{}
			continue
		}
		if uuid == "" {
			continue
		}
		c := cards[uuid]
		switch {
		case nvlinkSpeedRE.MatchString(line):
			c.Active++
			c.Total++
		case strings.HasPrefix(line, "Link ") && strings.Contains(line, "<inactive>"):
			c.Total++
		default:
			if m := nvlinkErrRE.FindStringSubmatch(line); m != nil {
				n, _ := strconv.ParseUint(m[2], 10, 64)
				switch m[1] {
				case "Replay":
					c.Replay += n
				case "Recovery":
					c.Recovery += n
				case "CRC":
					c.CRC += n
				}
			}
		}
		cards[uuid] = c
	}
	return cards
}
