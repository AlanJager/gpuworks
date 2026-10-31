package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// NVLinks that went down. A card's link count is compared with the most it
// has had since the agent started: fewer for nvlinkRounds readings in a row
// is a down link (a warning — the card works, multi-card work on it is
// slower), as many again for as long clears it. Either change has the next
// probe read the topology afresh, which is how the scheduler, choosing cards
// that are fully NVLink-connected, comes to avoid it. Error counters are
// evidence only; NVLink errors serious enough to matter arrive as Xid 74.
const nvlinkRounds = 2

type nvlinkWatch struct {
	mu        sync.Mutex
	lastRead  time.Time
	most      map[string]int
	low, back map[string]int
	down      map[string][]accelerator.HealthField
}

func (n *NodeRuntime) applyNVLinkFindings(ctx context.Context, gpus []proto.GPUInfo) {
	reporter, ok := n.prober.(gpu.NVLinkReporter)
	if !ok {
		return
	}
	cards, at, ok := reporter.NVLinks(ctx)
	w := &n.nvlink
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.most == nil {
		w.most, w.low, w.back = map[string]int{}, map[string]int{}, map[string]int{}
		w.down = map[string][]accelerator.HealthField{}
	}
	fresh := ok && at.After(w.lastRead)
	if fresh {
		w.lastRead = at
	}
	changed := false
	for i := range gpus {
		card := &gpus[i]
		ident := accelerator.CardIdent(card.UUID, card.PCIeBDF)
		if ident == "" || !accelerator.CardUsable(card.Ownership) {
			continue
		}
		if c, seen := cards[card.UUID]; fresh && seen {
			if c.Active > w.most[ident] {
				w.most[ident] = c.Active
			}
			if c.Active < w.most[ident] {
				w.low[ident]++
				w.back[ident] = 0
				if _, already := w.down[ident]; !already && w.low[ident] >= nvlinkRounds {
					slog.Warn("NVLink down", "card", card.PCIeBDF, "active", c.Active, "of", w.most[ident])
					changed = true
				}
				if _, already := w.down[ident]; already || w.low[ident] >= nvlinkRounds {
					w.down[ident] = []accelerator.HealthField{{Name: "NVLink active links", Value: fmt.Sprintf("%d of %d", c.Active, w.most[ident])},
						{Name: "NVLink errors", Value: fmt.Sprintf("replay %d, recovery %d, CRC %d", c.Replay, c.Recovery, c.CRC)}}
				}
			} else {
				w.back[ident]++
				w.low[ident] = 0
				if _, was := w.down[ident]; was && w.back[ident] >= nvlinkRounds {
					slog.Info("NVLink back up", "card", card.PCIeBDF, "active", c.Active)
					delete(w.down, ident)
					changed = true
				}
			}
		}
		evidence, down := w.down[ident]
		if !down || accelerator.MostSevereHealth(card.HealthCode, accelerator.HealthNVLinkDown) != accelerator.HealthNVLinkDown {
			continue
		}
		gpu.SetCardFinding(card, accelerator.HealthNVLinkDown, evidence)
	}
	if changed {
		reporter.RefreshTopology()
	}
}
