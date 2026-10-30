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

// Fresh is how long a note stands without the controller noting it again;
// the controller plans a waiting replica far more often (every few seconds),
// so a note older than this is one it no longer makes: the replica was
// placed, or no longer waits.
const Fresh = 30 * time.Second

// Waiter is a deployment whose replica waits for the holders' instances to
// sleep.
type Waiter struct {
	DeploymentID uint
	// Instances are the awake instances it waits for.
	Instances []string
	// Since is when it started waiting; Seen when it was last noted.
	Since, Seen time.Time
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

// Wait notes that the deployment's replica waits for instances, at now. A
// deployment already waiting keeps when it started.
func (r *Registry) Wait(deploymentID uint, instances []string, now time.Time) {
	r.mu.Lock()
	w, ok := r.m[deploymentID]
	if !ok {
		w = Waiter{DeploymentID: deploymentID, Since: now}
	}
	w.Instances, w.Seen = slices.Clone(instances), now
	r.m[deploymentID] = w
	notify := r.onWait
	r.mu.Unlock()
	if !ok && notify != nil {
		notify(deploymentID)
	}
}

// Done ends the deployment's wait: its replica was placed, or it is gone.
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
		w.Instances = slices.Clone(w.Instances)
		out = append(out, w)
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
	w.Instances = slices.Clone(w.Instances)
	return w, true
}
