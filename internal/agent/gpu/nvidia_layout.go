package gpu

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The answers below describe how the cards are laid out, not what they are
// doing, so they are asked again only when the cards change, or once
// nvidiaLayoutRecheck has passed (an NVLink that went down shows in the
// topology without the inventory changing). Asking them with every status
// report cost more than the report: `nvidia-smi topo -m` takes 11-31 s even
// on a healthy two-card host (gpu3), and while it ran every other query
// queued behind it and the CLI counted as slow.
const (
	// nvidiaLayoutRecheck bounds how long a layout answer is kept.
	nvidiaLayoutRecheck = time.Hour
	// nvidiaPCIeInterval is how often the PCIe throughput sample
	// (`nvidia-smi dmon`) is added to the metrics; the samples between carry
	// none, rather than repeating an old one as new.
	nvidiaPCIeInterval = 60 * time.Second
)

// layoutAnswer is one cached layout query.
type layoutAnswer struct {
	out       string
	inventory string // nvidiaInventoryKey of the cards it was asked about
	at        time.Time
}

// nvidiaLayout holds the layout answers of one prober.
type nvidiaLayout struct {
	mu        sync.Mutex
	inventory string // the cards the last successful probe listed
	topology  layoutAnswer
	listing   layoutAnswer // `nvidia-smi -L`
	pcieAt    time.Time    // when dmon was last asked
}

// nvidiaInventoryKey names the cards nvidia-smi listed: which card sits at
// which index and address.
func nvidiaInventoryKey(gpus []proto.GPUInfo) string {
	var b strings.Builder
	for _, g := range gpus {
		b.WriteString(strconv.Itoa(g.Index))
		b.WriteByte('/')
		b.WriteString(g.UUID)
		b.WriteByte('/')
		b.WriteString(g.PCIeBDF)
		b.WriteByte(';')
	}
	return b.String()
}

func (p *SMIProber) rememberInventory(gpus []proto.GPUInfo) string {
	key := nvidiaInventoryKey(gpus)
	p.layout.mu.Lock()
	p.layout.inventory = key
	p.layout.mu.Unlock()
	return key
}

// layoutQuery answers a layout query for the cards in inventory: from the
// cache while it describes those cards and is younger than
// nvidiaLayoutRecheck, otherwise by asking. A slow CLI is not asked; the
// cached answer is still served when it describes the same cards. ok is false
// when there is no answer for these cards.
func (p *SMIProber) layoutQuery(ctx context.Context, slot *layoutAnswer, inventory string, args ...string) (string, bool) {
	p.layout.mu.Lock()
	cached := *slot
	p.layout.mu.Unlock()
	same := cached.out != "" && cached.inventory == inventory
	if same && p.clockNow().Sub(cached.at) < nvidiaLayoutRecheck {
		return cached.out, true
	}
	if p.slow() {
		return cached.out, same
	}
	out, err := p.query(ctx, args...)
	if err != nil || strings.TrimSpace(out) == "" {
		return cached.out, same
	}
	p.layout.mu.Lock()
	*slot = layoutAnswer{out: out, inventory: inventory, at: p.clockNow()}
	p.layout.mu.Unlock()
	return out, true
}

// pcieDue reports whether this metrics call should ask dmon, and if so
// marks it asked. A failed dmon waits the interval too: asking it every
// tick would cost what the interval saves.
func (p *SMIProber) pcieDue() bool {
	p.layout.mu.Lock()
	defer p.layout.mu.Unlock()
	now := p.clockNow()
	if !p.layout.pcieAt.IsZero() && now.Sub(p.layout.pcieAt) < nvidiaPCIeInterval {
		return false
	}
	p.layout.pcieAt = now
	return true
}
