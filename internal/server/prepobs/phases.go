package prepobs

import (
	"sync"
	"time"
)

// The phases the controller itself performs before an agent copy exists.
const (
	PhaseEvicting       = "evicting"
	PhaseEvictionFailed = "eviction_failed"
)

const (
	// PhaseTTL is how long a controller phase stays visible without renewal.
	PhaseTTL = time.Minute
	// PhaseLimit bounds the phases observed at once.
	PhaseLimit = 4096
)

// Phases is a producer's latest phase per key, bounded and expiring.
type Phases[K comparable] struct {
	mu      sync.Mutex
	entries *expiring[K, string]
}

func NewPhases[K comparable]() *Phases[K] {
	return &Phases[K]{entries: newExpiring[K, string](PhaseTTL, PhaseLimit)}
}

// Set records a phase; an empty phase retires the key.
func (p *Phases[K]) Set(key K, phase string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if phase == "" {
		p.entries.remove(key)
		return
	}
	p.entries.put(key, phase, now)
}

func (p *Phases[K]) Get(key K, now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	phase, _ := p.entries.get(key, now)
	return phase
}
