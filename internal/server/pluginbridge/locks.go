package pluginbridge

import (
	"context"
	"sync"
	"time"
)

// gateCancelWait is how long a disable or uninstall of a package waits for
// an upgrade of that package to finish before it cancels the upgrade's
// health gate, which rolls the upgrade back and releases the package.
const gateCancelWait = 10 * time.Second

// idLocks serializes the administration calls on each package id (install,
// upgrade, rollback, enable, disable, restart, uninstall, overrides, the
// impact preview) and Run's start of it; calls on different ids run side by
// side, so an upgrade waiting out its health gate holds up only its own
// package. An entry exists only while a call holds or waits for its id.
type idLocks struct {
	mu sync.Mutex
	m  map[string]*idLock
}

type idLock struct {
	sem  chan struct{} // one slot: held while a call runs
	refs int           // calls holding or waiting
	// cancelGate, set while an upgrade of the id waits out its health
	// gate, ends that wait: the upgrade then rolls back.
	cancelGate context.CancelFunc
}

func (l *idLocks) ref(id string) *idLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]*idLock{}
	}
	k := l.m[id]
	if k == nil {
		k = &idLock{sem: make(chan struct{}, 1)}
		l.m[id] = k
	}
	k.refs++
	return k
}

func (l *idLocks) unref(id string, k *idLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if k.refs--; k.refs == 0 {
		delete(l.m, id)
	}
}

// lock takes id's lock, waiting as long as it takes; the returned function
// releases it.
func (l *idLocks) lock(id string) func() {
	k := l.ref(id)
	k.sem <- struct{}{}
	return l.releaser(id, k)
}

// lockCancelling takes id's lock like lock, but once it has waited wait it
// cancels the health gate of an upgrade holding it (as soon as that gate
// runs), then waits on for the upgrade's rollback to finish.
func (l *idLocks) lockCancelling(id string, wait time.Duration) func() {
	k := l.ref(id)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case k.sem <- struct{}{}:
		return l.releaser(id, k)
	case <-timer.C:
	}
	// The upgrade may still be stopping the old version or copying its
	// data; its gate is cancelled as soon as it starts.
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		l.mu.Lock()
		cancel := k.cancelGate
		l.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		select {
		case k.sem <- struct{}{}:
			return l.releaser(id, k)
		case <-tick.C:
		}
	}
}

func (l *idLocks) releaser(id string, k *idLock) func() {
	return sync.OnceFunc(func() {
		<-k.sem
		l.unref(id, k)
	})
}

// gate registers cancel as the way to end the health gate of the upgrade
// holding id's lock; the returned function unregisters it. The caller
// holds id's lock.
func (l *idLocks) gate(id string, cancel context.CancelFunc) func() {
	l.mu.Lock()
	k := l.m[id]
	if k != nil {
		k.cancelGate = cancel
	}
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		if k != nil {
			k.cancelGate = nil
		}
		l.mu.Unlock()
	}
}

// cancelGates ends every health gate under way, for a server that stops.
func (l *idLocks) cancelGates() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range l.m {
		if k.cancelGate != nil {
			k.cancelGate()
		}
	}
}
