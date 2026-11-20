package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// countingClusterDriver is an account whose cluster listing can fail and
// whose calls are counted.
type countingClusterDriver struct {
	clusterTestDriver
	mu    sync.Mutex
	calls int
	err   error
}

func (*countingClusterDriver) Name() string { return "counting-cluster-provider" }

func (d *countingClusterDriver) ListClusters(ctx context.Context, creds Credentials) ([]Cluster, error) {
	d.mu.Lock()
	d.calls++
	err := d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return d.clusterTestDriver.ListClusters(ctx, creds)
}

func (d *countingClusterDriver) listed() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func TestCachedClustersAsksTheCloudOncePerMaxAge(t *testing.T) {
	drv := &countingClusterDriver{}
	svc, p := clusterService(t, drv)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		clusters, err := svc.CachedClusters(ctx, p.ID, time.Minute)
		if err != nil || len(clusters) != 1 || clusters[0].ExternalID != "c-1" {
			t.Fatalf("CachedClusters = %+v, %v", clusters, err)
		}
	}
	if drv.listed() != 1 {
		t.Fatalf("the cloud was asked %d times for four reads inside the age", drv.listed())
	}
	// Older than the caller allows: listed again.
	if _, err := svc.CachedClusters(ctx, p.ID, 0); err != nil || drv.listed() != 2 {
		t.Fatalf("a zero max age must list again: calls %d, err %v", drv.listed(), err)
	}
	// The live listing is not the cache's business.
	if _, err := svc.Clusters(ctx, p.ID); err != nil || drv.listed() != 3 {
		t.Fatalf("Clusters is always live: calls %d, err %v", drv.listed(), err)
	}
}

// A cloud that fails is not asked again on every poll.
func TestCachedClustersRemembersAFailureBriefly(t *testing.T) {
	drv := &countingClusterDriver{err: errors.New("aliyun: Forbidden.RAM")}
	svc, p := clusterService(t, drv)
	for i := 0; i < 3; i++ {
		if _, err := svc.CachedClusters(context.Background(), p.ID, time.Minute); err == nil || err.Error() != "aliyun: Forbidden.RAM" {
			t.Fatalf("CachedClusters = %v, want the cloud's error each time", err)
		}
	}
	if drv.listed() != 1 {
		t.Fatalf("a failing cloud was asked %d times; the failure is remembered for %s", drv.listed(), failedClusterListTTL)
	}
}

// A caller that gave up (its context ended) does not take the read down with
// it: the cloud's answer is kept for the next caller.
func TestCachedClustersKeepsTheReadOfACallerThatGaveUp(t *testing.T) {
	drv := &countingClusterDriver{}
	svc, p := clusterService(t, drv)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := svc.CachedClusters(cancelled, p.ID, time.Minute); err != nil && !errors.Is(err, ErrListingPending) {
		t.Fatalf("a cancelled caller got %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		clusters, err := svc.CachedClusters(context.Background(), p.ID, time.Minute)
		if err == nil && len(clusters) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the read of the cancelled caller was lost: %+v, %v", clusters, err)
		}
	}
	if drv.listed() != 1 {
		t.Fatalf("the cloud was asked %d times; the cancelled caller's read serves everybody", drv.listed())
	}
}

// gatedFetch is a read of the cloud that stays out until opened.
type gatedFetch struct {
	mu    sync.Mutex
	calls int
	gate  chan struct{}
	value []string
	err   error
}

func (g *gatedFetch) fetch(ctx context.Context) ([]string, error) {
	g.mu.Lock()
	g.calls++
	gate, value, err := g.gate, g.value, g.err
	g.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return value, err
}

func (g *gatedFetch) asked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// Callers that arrive while the cloud is slow share one read, each waits only
// as long as its own context lets it, and the read is not theirs: once it
// ends, everyone has its answer.
func TestListCacheSharesOneSlowReadAndDoesNotTieItToItsCallers(t *testing.T) {
	var cache listCache[[]string]
	g := &gatedFetch{gate: make(chan struct{}), value: []string{"c-1"}}

	const callers = 8
	results := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := cache.get(ctx, 1, time.Minute, g.fetch)
			if time.Since(start) > 2*time.Second {
				err = errors.New("a caller waited far beyond its own context")
			}
			results <- err
		}()
	}
	for i := 0; i < callers; i++ {
		if err := <-results; !errors.Is(err, ErrListingPending) {
			t.Fatalf("a caller of a slow cloud got %v, want ErrListingPending", err)
		}
	}
	if g.asked() != 1 {
		t.Fatalf("%d concurrent callers made %d reads of the cloud", callers, g.asked())
	}
	close(g.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := cache.get(context.Background(), 1, time.Minute, g.fetch)
		if err == nil && len(v) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the slow read never landed: %v, %v", v, err)
		}
	}
	if g.asked() != 1 {
		t.Fatalf("the cloud was read %d times", g.asked())
	}
}

// No lock is held while the cloud is read: a read stuck on one account does
// not hold up another account, nor the eviction of the stuck one.
func TestListCacheHoldsNoLockWhileTheCloudIsRead(t *testing.T) {
	var cache listCache[[]string]
	stuck := &gatedFetch{gate: make(chan struct{})}
	defer close(stuck.gate)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = cache.get(ctx, 1, time.Minute, stuck.fetch)
	}()
	for stuck.asked() == 0 {
		time.Sleep(time.Millisecond)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		other := &gatedFetch{value: []string{"other"}}
		if v, err := cache.get(context.Background(), 2, time.Minute, other.fetch); err != nil || len(v) != 1 {
			t.Errorf("another account: %v, %v", v, err)
		}
		cache.evict(1)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a read of one account blocked another account or its eviction")
	}
}

// While the cloud cannot be read the last good listing is the answer, until it
// is too old to say anything; with none, the failure.
func TestListCacheGivesTheLastGoodListingWhileTheCloudFails(t *testing.T) {
	var cache listCache[[]string]
	g := &gatedFetch{value: []string{"c-1"}}
	if v, err := cache.get(context.Background(), 1, 0, g.fetch); err != nil || len(v) != 1 {
		t.Fatalf("first read: %v, %v", v, err)
	}
	g.mu.Lock()
	g.err = errors.New("cloud down")
	g.mu.Unlock()
	if v, err := cache.get(context.Background(), 1, 0, g.fetch); err != nil || len(v) != 1 || v[0] != "c-1" {
		t.Fatalf("while the cloud fails: %v, %v; want the last good listing", v, err)
	}
	if g.asked() != 2 {
		t.Fatalf("asked %d times", g.asked())
	}
	// Too old to be given.
	cache.mu.Lock()
	cache.entries[1].at = time.Now().Add(-listStaleLimit - time.Minute)
	cache.mu.Unlock()
	if _, err := cache.get(context.Background(), 1, 0, g.fetch); err == nil || err.Error() != "cloud down" {
		t.Fatalf("a stale listing was given: %v", err)
	}
}

// What was listed with an account's old credentials, or before it was
// deleted, is not given afterwards.
func TestListCacheEvictForgetsTheAccount(t *testing.T) {
	var cache listCache[[]string]
	g := &gatedFetch{value: []string{"old"}}
	if v, _ := cache.get(context.Background(), 1, time.Minute, g.fetch); len(v) != 1 || v[0] != "old" {
		t.Fatalf("v = %v", v)
	}
	cache.evict(1)
	g.mu.Lock()
	g.value = []string{"new"}
	g.mu.Unlock()
	if v, err := cache.get(context.Background(), 1, time.Minute, g.fetch); err != nil || len(v) != 1 || v[0] != "new" || g.asked() != 2 {
		t.Fatalf("after evict: %v, %v, asked %d", v, err, g.asked())
	}
}

// A read that is still out when its account is evicted lands in an entry nobody
// reaches: it must not become the new credentials' listing.
func TestListCacheEvictedReadDoesNotLandInTheNewEntry(t *testing.T) {
	var cache listCache[[]string]
	old := &gatedFetch{gate: make(chan struct{}), value: []string{"old"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, _ = cache.get(ctx, 1, time.Minute, old.fetch)
	cache.evict(1)
	close(old.gate)
	time.Sleep(50 * time.Millisecond)
	fresh := &gatedFetch{value: []string{"new"}}
	if v, err := cache.get(context.Background(), 1, time.Minute, fresh.fetch); err != nil || len(v) != 1 || v[0] != "new" {
		t.Fatalf("after evict: %v, %v", v, err)
	}
}

// Replacing an account's credentials, or deleting the account, drops its
// listings through the service.
func TestForgetAccountDropsBothListings(t *testing.T) {
	svc, providerID, drv := newHPCListingService(t, "hpc-forget")
	for i := 0; i < 2; i++ {
		if _, err := svc.CachedHPCClusters(t.Context(), providerID, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if drv.asked() != 1 {
		t.Fatalf("asked %d times", drv.asked())
	}
	svc.ForgetAccount(providerID)
	if _, err := svc.CachedHPCClusters(t.Context(), providerID, time.Minute); err != nil || drv.asked() != 2 {
		t.Fatalf("after ForgetAccount: asked %d, %v", drv.asked(), err)
	}
}

// New credentials must not be answered with what the old ones listed.
func TestUpdatingTheCredentialsDropsTheAccountsListing(t *testing.T) {
	drv := &countingClusterDriver{}
	svc, p := clusterService(t, drv)
	for i := 0; i < 2; i++ {
		if _, err := svc.CachedClusters(context.Background(), p.ID, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if drv.listed() != 1 {
		t.Fatalf("listed %d times before the update", drv.listed())
	}
	if _, err := svc.Update(context.Background(), p.ID, "acct", Credentials{APIKey: "k2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CachedClusters(context.Background(), p.ID, time.Minute); err != nil || drv.listed() != 2 {
		t.Fatalf("after the update: listed %d times, %v", drv.listed(), err)
	}
}
