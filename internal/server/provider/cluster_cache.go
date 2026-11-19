package provider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// failedClusterListTTL is how long a failed listing is remembered, so a cloud
// that is down is asked about once in a while and not on every poll.
const failedClusterListTTL = 30 * time.Second

// listStaleLimit is how old the last good listing may be and still be given to
// a caller while the cloud cannot be read (or is slow): a cluster list that
// old says nothing about the cloud any more.
const listStaleLimit = 10 * time.Minute

// cloudListTimeout bounds one read of an account's clusters from the cloud.
// The read is not the caller's: a caller that stops waiting leaves it running,
// so the answer is there for the next one. A var so tests can shorten it.
var cloudListTimeout = 90 * time.Second

// ErrListingPending: the account's clusters have not been read from the cloud
// yet (the read is still going on) and there is no earlier listing to give.
var ErrListingPending = errors.New("the cloud's cluster listing has not been read yet")

// listCache keeps each account's last listing of one kind of cluster for
// callers that need a fact about the account's clusters on a hot path (the
// cluster entries' lists are polled by every open page) and can live with it
// being a little old.
//
// The cloud is read by at most one goroutine per account at a time, under its
// own timeout and not under any caller's context, and no lock is held while
// it is read: callers wait for that read as long as their own context allows,
// then take the last good listing, or ErrListingPending when there is none.
type listCache[V any] struct {
	mu      sync.Mutex
	entries map[uint]*listCacheEntry[V]
}

type listCacheEntry[V any] struct {
	have  bool // value is a good listing taken at at
	at    time.Time
	value V
	// err and errAt are the last read that failed, cleared by a good one.
	err    error
	errAt  time.Time
	flight *listFlight
}

// listFlight is a read of the cloud that is going on.
type listFlight struct{ done chan struct{} }

func (c *listCache[V]) entry(providerID uint) *listCacheEntry[V] {
	if c.entries == nil {
		c.entries = map[uint]*listCacheEntry[V]{}
	}
	e := c.entries[providerID]
	if e == nil {
		e = &listCacheEntry[V]{}
		c.entries[providerID] = e
	}
	return e
}

// evict forgets everything about an account: its credentials changed or it was
// deleted, so what was listed with the old ones says nothing about it. A read
// still going on finishes into an entry nobody reaches any more.
func (c *listCache[V]) evict(providerID uint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, providerID)
}

// get answers the account's listing when it is younger than maxAge; otherwise
// it has the cloud read (once, however many callers ask) and waits for that
// until ctx ends. A failed read is remembered briefly, error included. When
// the read does not finish in time or fails, the last good listing younger
// than listStaleLimit is the answer; with none, the failure, or
// ErrListingPending while the read goes on.
func (c *listCache[V]) get(ctx context.Context, providerID uint, maxAge time.Duration, fetch func(context.Context) (V, error)) (V, error) {
	c.mu.Lock()
	e := c.entry(providerID)
	now := time.Now()
	if e.have && now.Sub(e.at) < maxAge {
		v := e.value
		c.mu.Unlock()
		return v, nil
	}
	if e.err != nil && now.Sub(e.errAt) < min(maxAge, failedClusterListTTL) && e.flight == nil {
		v, err := c.fallbackLocked(e, now)
		c.mu.Unlock()
		return v, err
	}
	f := e.flight
	if f == nil {
		f = &listFlight{done: make(chan struct{})}
		e.flight = f
		go c.read(e, f, fetch)
	}
	c.mu.Unlock()

	select {
	case <-f.done:
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fallbackLocked(e, time.Now())
}

// fallbackLocked is what a caller gets when the read it wanted is not a
// success it can use: the last good listing if it is not too old, else the
// last failure, else ErrListingPending. A good listing the read just stored
// comes out of here as well, being the last good one.
func (c *listCache[V]) fallbackLocked(e *listCacheEntry[V], now time.Time) (V, error) {
	if e.have && now.Sub(e.at) < listStaleLimit {
		return e.value, nil
	}
	var zero V
	if e.err != nil {
		return zero, e.err
	}
	return zero, ErrListingPending
}

// read is the one read of the cloud for an account, run apart from any caller.
func (c *listCache[V]) read(e *listCacheEntry[V], f *listFlight, fetch func(context.Context) (V, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), cloudListTimeout)
	defer cancel()
	v, err := func() (v V, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("listing the account's clusters panicked: %v", r)
			}
		}()
		return fetch(ctx)
	}()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		e.have, e.at, e.value, e.err = true, time.Now(), v, nil
	} else {
		e.err, e.errAt = err, time.Now()
	}
	e.flight = nil
	close(f.done)
}

// CachedClusters is Clusters answered from the account's last listing when it
// is younger than maxAge (see listCache for what a slow or failing cloud gives).
func (s *Service) CachedClusters(ctx context.Context, providerID uint, maxAge time.Duration) ([]Cluster, error) {
	return s.clusterLists.get(ctx, providerID, maxAge, func(ctx context.Context) ([]Cluster, error) { return s.Clusters(ctx, providerID) })
}

// CachedHPCClusters is HPCClusters answered from the account's last listing
// when it is younger than maxAge (see listCache).
func (s *Service) CachedHPCClusters(ctx context.Context, providerID uint, maxAge time.Duration) (HPCClusterList, error) {
	return s.hpcLists.get(ctx, providerID, maxAge, func(ctx context.Context) (HPCClusterList, error) { return s.HPCClusters(ctx, providerID) })
}

// ForgetAccount drops what the service remembers about an account's clusters:
// call it when its credentials were replaced or it was deleted.
func (s *Service) ForgetAccount(providerID uint) {
	s.clusterLists.evict(providerID)
	s.hpcLists.evict(providerID)
}
