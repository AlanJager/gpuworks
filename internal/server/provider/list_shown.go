package provider

import (
	"context"
	"time"
)

// A listing a page shows (an account's E-HPC clusters) is read from dozens of
// regional endpoints, which takes seconds when the connections are cold. The
// page is not made to wait for that every time: it gets the last listing at
// once and the cloud is read again behind it.

// listShownLimit is how old the last listing may be and still be shown while
// the cloud is read again; the page says when it was read.
const listShownLimit = 24 * time.Hour

// hpcShownRefreshAfter is how old the shown listing may be before a page's
// read has the cloud read again; hpcShownWait is how long that read is waited
// for before the old listing is answered, so a quick cloud answers fresh.
const (
	hpcShownRefreshAfter = 15 * time.Second
	hpcShownWait         = time.Second
)

// shown answers the account's last good listing when there is one no older
// than listShownLimit, with when it was read and whether a read of the cloud
// is going on to replace it. A listing older than refreshAfter has the cloud
// read again (once, however many callers ask); the caller waits up to wait
// for it, and gets the new listing when it lands in time. With no listing the
// caller waits for the read as long as ctx lets it, as get does. A failed read
// is remembered briefly, and the cloud is not asked again meanwhile.
func (c *listCache[V]) shown(ctx context.Context, providerID uint, refreshAfter, wait time.Duration, fetch func(context.Context) (V, error)) (v V, at time.Time, refreshing bool, err error) {
	c.mu.Lock()
	e := c.entry(providerID)
	now := time.Now()
	if e.have && now.Sub(e.at) < refreshAfter {
		v, at = e.value, e.at
		c.mu.Unlock()
		return v, at, false, nil
	}
	recentFailure := e.err != nil && now.Sub(e.errAt) < failedClusterListTTL && e.flight == nil
	if !recentFailure && e.flight == nil {
		e.flight = &listFlight{done: make(chan struct{})}
		go c.read(e, e.flight, fetch)
	}
	f, have := e.flight, e.have
	c.mu.Unlock()

	if f != nil {
		waitCtx := ctx
		if have {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, wait)
			defer cancel()
		}
		select {
		case <-f.done:
		case <-waitCtx.Done():
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	refreshing = e.flight != nil
	if e.have && time.Since(e.at) < listShownLimit {
		return e.value, e.at, refreshing, nil
	}
	var zero V
	if e.err != nil {
		return zero, time.Time{}, refreshing, e.err
	}
	return zero, time.Time{}, refreshing, ErrListingPending
}

// ShownHPCClusters is the account's HPC cluster listing for a page: the last
// one at once, with when it was read and whether a newer one is being read
// (see listCache.shown). fresh waits for a new read as long as ctx lets it,
// as after the account was synced.
func (s *Service) ShownHPCClusters(ctx context.Context, providerID uint, fresh bool) (list HPCClusterList, at time.Time, refreshing bool, err error) {
	refreshAfter, wait := hpcShownRefreshAfter, hpcShownWait
	if fresh {
		refreshAfter, wait = 0, cloudListTimeout
	}
	return s.hpcLists.shown(ctx, providerID, refreshAfter, wait, func(ctx context.Context) (HPCClusterList, error) { return s.HPCClusters(ctx, providerID) })
}
