// Package placewait holds the deployments whose next replica waits for
// others to give up cards (docs/explanation/service-levels.md): the
// controller notes each one every time it plans the replica and finds it
// waiting, and the residency arbiter reads them as it reads requests held for
// a sleeping deployment. It is memory only: after a restart the controller
// notes them again on its first pass.
package placewait

import (
	"slices"
	"sync"
	"time"
)

// Recheck is how often the controller plans a waiting replica again, noting
// it each time.
const Recheck = 5 * time.Second

// Fresh is how long a note stands without the controller noting it again:
// several Rechecks, so a note older than this is one the controller no
// longer makes (the replica was placed, or no longer waits), not one a slow
// pass has yet to renew.
const Fresh = 6 * Recheck

// Cards are the cards on one node a waiting replica would take.
type Cards struct {
	NodeID     uint
	GPUIndexes []int
}

// Takes says the replica would take card index of node.
func (c Cards) Takes(nodeID uint, index int) bool {
	return c.NodeID == nodeID && slices.Contains(c.GPUIndexes, index)
}

// Waiter is a deployment whose replica waits for the cards it would take to
// be given up.
type Waiter struct {
	DeploymentID uint
	// Cards are the cards it waits for. They stay its once the holders'
	// sleeps are ordered: a sleeper's wake does not take them back while it
	// waits (servicelevel.KeepsCardsAgainst).
	Cards Cards
	// Holders are the awake instances on them whose sleep is not ordered
	// yet: what the residency arbiter puts to sleep. None is a wait for
	// sleeps already ordered to land.
	Holders []string
	// Since is when it started waiting; Seen when it was last noted.
	Since, Seen time.Time
}

func (w Waiter) clone() Waiter {
	w.Holders = slices.Clone(w.Holders)
	w.Cards.GPUIndexes = slices.Clone(w.Cards.GPUIndexes)
	return w
}

// Registry is the waiting deployments. The zero value is not usable; New.
type Registry struct {
	mu sync.Mutex
	m  map[uint]Waiter
	// onWait is told of a deployment that starts waiting.
	onWait func(deploymentID uint)
}

// OnWait sets what is told of a deployment that starts waiting: the
// residency arbiter, so it looks at once rather than on its next tick.
func (r *Registry) OnWait(f func(deploymentID uint)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onWait = f
}

// New is an empty registry.
func New() *Registry { return &Registry{m: map[uint]Waiter{}} }

// Wait notes that the deployment's replica waits for cards and the awake
// holders on them, at now. A deployment already waiting keeps when it
// started.
func (r *Registry) Wait(deploymentID uint, cards Cards, holders []string, now time.Time) {
	r.mu.Lock()
	w, ok := r.m[deploymentID]
	if !ok {
		w = Waiter{DeploymentID: deploymentID, Since: now}
	}
	w.Cards, w.Holders, w.Seen = cards, holders, now
	r.m[deploymentID] = w.clone()
	notify := r.onWait
	r.mu.Unlock()
	if !ok && notify != nil {
		notify(deploymentID)
	}
}

// Done ends the deployment's wait: its replica was placed, no longer waits
// for others' cards, or it is gone.
func (r *Registry) Done(deploymentID uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, deploymentID)
}

// Waiting are the deployments waiting at now, oldest first; a note older
// than Fresh is dropped.
func (r *Registry) Waiting(now time.Time) []Waiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Waiter, 0, len(r.m))
	for id, w := range r.m {
		if now.Sub(w.Seen) >= Fresh {
			delete(r.m, id)
			continue
		}
		out = append(out, w.clone())
	}
	slices.SortFunc(out, func(a, b Waiter) int { return a.Since.Compare(b.Since) })
	return out
}

// Get is the deployment's wait at now, if it waits.
func (r *Registry) Get(deploymentID uint, now time.Time) (Waiter, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.m[deploymentID]
	if !ok || now.Sub(w.Seen) >= Fresh {
		return Waiter{}, false
	}
	return w.clone(), true
}
