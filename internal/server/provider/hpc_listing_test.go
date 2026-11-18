package provider

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// hpcListingDriver is an account that runs commands on E-HPC manager nodes
// and lists its HPC clusters live, a cluster with no synced node included. A
// listing can be made to fail, to answer a given result, or to stay out until
// release is closed (a slow cloud).
type hpcListingDriver struct {
	applyServiceDriver
	mu       sync.Mutex
	listErr  error
	result   *HPCClusterList
	release  chan struct{}
	listings int
}

func (d *hpcListingDriver) ListHPCClusters(ctx context.Context, _ Credentials) (HPCClusterList, error) {
	d.mu.Lock()
	d.listings++
	listErr, result, release := d.listErr, d.result, d.release
	d.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return HPCClusterList{}, ctx.Err()
		}
	}
	if listErr != nil {
		return HPCClusterList{}, listErr
	}
	if result != nil {
		return *result, nil
	}
	return HPCClusterList{Clusters: []HPCClusterListing{
		{HPCCluster: HPCCluster{ID: "ehpc-sz-9", Name: "empty", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true},
		{HPCCluster: HPCCluster{ID: "ehpc-bj-1", Name: "bj", Scheduler: "slurm"}, Region: "cn-beijing", State: "running", Running: true},
		{HPCCluster: HPCCluster{ID: "ehpc-sz-pbs", Name: "pbs", Scheduler: "pbs"}, Region: "cn-shenzhen", State: "running", Running: true},
	}}, nil
}

func (d *hpcListingDriver) asked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.listings
}

func (d *hpcListingDriver) set(f func(*hpcListingDriver)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(d)
}

func newHPCListingService(t *testing.T, name string) (*Service, uint, *hpcListingDriver) {
	t.Helper()
	driver := &hpcListingDriver{applyServiceDriver: applyServiceDriver{
		hpcTestDriver: hpcTestDriver{channelTestDriver: channelTestDriver{fallbackTestDriver: fallbackTestDriver{name: name}}},
		instances:     []Instance{computeNode("i-0", "ehpc-sz-1_gpu000", "ehpc-sz-1", "slurm", 1)},
	}}
	svc, providerID := newFallbackService(t, &driver.fallbackTestDriver)
	Register(driver)
	if _, err := svc.Sync(t.Context(), providerID); err != nil {
		t.Fatal(err)
	}
	return svc, providerID, driver
}

// The service lists the account's clusters in a stable order (region, then
// ID), and says so for a kind that has none rather than answer empty.
func TestHPCClustersAreListedInRegionOrderAndAKindWithoutThemSaysSo(t *testing.T) {
	svc, providerID, _ := newHPCListingService(t, "hpc-listing")
	got, err := svc.HPCClusters(t.Context(), providerID)
	if err != nil || len(got.Clusters) != 3 || got.Clusters[0].ID != "ehpc-bj-1" || got.Clusters[1].ID != "ehpc-sz-9" || got.Clusters[2].ID != "ehpc-sz-pbs" {
		t.Fatalf("HPCClusters = %+v, %v", got, err)
	}
	if !svc.CanListHPCClusters("hpc-listing") || svc.CanListHPCClusters("no-such-kind") {
		t.Fatal("CanListHPCClusters does not follow the driver")
	}

	plain, plainID, _ := newHPCService(t, "hpc-listing-plain")
	if _, err := plain.HPCClusters(t.Context(), plainID); !errors.Is(err, ErrNoHPCClusters) || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a kind without HPC clusters: %v", err)
	}
	if plain.CanListHPCClusters("hpc-listing-plain") {
		t.Fatal("a driver that does not list HPC clusters says it does")
	}
}

func TestCachedHPCClustersAsksTheCloudOncePerMaxAgeAndRemembersAFailureBriefly(t *testing.T) {
	svc, providerID, drv := newHPCListingService(t, "hpc-listing-cache")
	for i := 0; i < 3; i++ {
		if got, err := svc.CachedHPCClusters(t.Context(), providerID, time.Minute); err != nil || len(got.Clusters) != 3 {
			t.Fatalf("CachedHPCClusters = %+v, %v", got, err)
		}
	}
	if drv.asked() != 1 {
		t.Fatalf("the cloud was asked %d times within the max age", drv.asked())
	}
	if _, err := svc.CachedHPCClusters(t.Context(), providerID, 0); err != nil || drv.asked() != 2 {
		t.Fatalf("a zero max age must ask again: asked %d, %v", drv.asked(), err)
	}

	failing, failingID, fdrv := newHPCListingService(t, "hpc-listing-cache-fail")
	fdrv.listErr = errors.New("cloud down")
	for i := 0; i < 3; i++ {
		if _, err := failing.CachedHPCClusters(t.Context(), failingID, time.Minute); err == nil || err.Error() != "cloud down" {
			t.Fatalf("CachedHPCClusters = %v, want the cloud's error each time", err)
		}
	}
	if fdrv.asked() != 1 {
		t.Fatalf("a failing cloud was asked %d times; the failure is remembered for %s", fdrv.asked(), failedClusterListTTL)
	}
}

// A cluster none of whose nodes was synced is placed by the cloud's listing:
// its region and scheduler come from there, and it has no compute node yet.
func TestSlurmApplyTargetPlacesAClusterWithoutSyncedNodesByTheCloudListing(t *testing.T) {
	svc, providerID, drv := newHPCListingService(t, "hpc-target-listing")
	target, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-9", time.Minute)
	if err != nil || target.Region != "cn-shenzhen" || target.ClusterID != "ehpc-sz-9" || len(target.ExpectGPU) != 0 {
		t.Fatalf("target = %+v, %v", target, err)
	}
	if target, err = svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-bj-1", time.Minute); err != nil || target.Region != "cn-beijing" {
		t.Fatalf("target = %+v, %v", target, err)
	}
	// A synced cluster is placed by its nodes, not by asking the cloud.
	asked := drv.asked()
	if target, err = svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-1", time.Minute); err != nil || target.Region != "cn-shenzhen" || len(target.ExpectGPU) != 1 {
		t.Fatalf("target = %+v, %v", target, err)
	}
	if drv.asked() != asked {
		t.Fatal("a cluster with synced nodes asked the cloud")
	}
	// Not Slurm, not in the listing.
	if _, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-pbs", time.Minute); notApplicableReason(t, err) != messages.SlurmApplyReasonNotSlurmCluster {
		t.Fatalf("pbs: %v", err)
	}
	if _, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-gone", time.Minute); notApplicableReason(t, err) != messages.SlurmApplyReasonClusterUnknown {
		t.Fatalf("unlisted: %v", err)
	}
}

// A listing that fails, a key that may not list, a region missing from the
// listing and a listing not read yet each have their own reason; none of them
// is "the cloud does not have this cluster".
func TestSlurmApplyTargetSaysWhyTheListingCannotPlaceACluster(t *testing.T) {
	t.Run("the listing failed", func(t *testing.T) {
		svc, providerID, drv := newHPCListingService(t, "hpc-target-failed")
		drv.set(func(d *hpcListingDriver) { d.listErr = errors.New("cloud down") })
		if _, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-9", 0); notApplicableReason(t, err) != messages.SlurmApplyReasonClusterListingFailed {
			t.Fatalf("listing failed: %v", err)
		}
		// A synced cluster does not need the listing.
		if _, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-1", 0); err != nil {
			t.Fatalf("synced cluster: %v", err)
		}
	})
	t.Run("the key may not list", func(t *testing.T) {
		svc, providerID, drv := newHPCListingService(t, "hpc-target-denied")
		drv.set(func(d *hpcListingDriver) {
			d.result = &HPCClusterList{Access: HPCAccessPermissionMissing, Permission: "ehpc:ListClusters"}
		})
		if _, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-9", 0); notApplicableReason(t, err) != messages.SlurmApplyReasonClusterListingFailed {
			t.Fatalf("denied: %v", err)
		}
	})
	t.Run("a region could not be listed", func(t *testing.T) {
		svc, providerID, drv := newHPCListingService(t, "hpc-target-partial")
		drv.set(func(d *hpcListingDriver) {
			d.result = &HPCClusterList{
				Clusters: []HPCClusterListing{{HPCCluster: HPCCluster{ID: "ehpc-bj-1", Scheduler: "slurm"}, Region: "cn-beijing", Running: true}},
				Failed:   []HPCRegionFailure{{Region: "cn-shenzhen", Code: "Throttling.User"}},
			}
		})
		if target, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-bj-1", 0); err != nil || target.Region != "cn-beijing" {
			t.Fatalf("a listed cluster: %+v, %v", target, err)
		}
		if _, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-9", 0); notApplicableReason(t, err) != messages.SlurmApplyReasonClusterListingFailed {
			t.Fatalf("a cluster that may be in the failed region: %v", err)
		}
	})
	t.Run("not read yet", func(t *testing.T) {
		svc, providerID, drv := newHPCListingService(t, "hpc-target-pending")
		release := make(chan struct{})
		drv.set(func(d *hpcListingDriver) { d.release = release })
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if _, err := svc.SlurmApplyTarget(ctx, providerID, "ehpc-sz-9", time.Minute); notApplicableReason(t, err) != messages.SlurmApplyReasonClusterListingPending {
			t.Fatalf("slow listing: %v", err)
		}
		// The read went on without the caller: the next one finds its answer.
		close(release)
		deadline := time.Now().Add(5 * time.Second)
		for {
			target, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-9", time.Minute)
			if err == nil && target.Region == "cn-shenzhen" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the listing never arrived: %+v, %v", target, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if drv.asked() != 1 {
			t.Fatalf("the cloud was asked %d times", drv.asked())
		}
	})
}

// One SlurmApplyTargets places every cluster of an account from one read of
// the account's listing, and counts the nodes with a card.
func TestSlurmApplyTargetsReadsTheListingOnceForAllTheClusters(t *testing.T) {
	svc, providerID, drv := newHPCListingService(t, "hpc-targets-once")
	reads := 0
	targets, err := svc.SlurmApplyTargets(providerID, func() (HPCClusterList, error) {
		reads++
		return svc.HPCClusters(t.Context(), providerID)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ehpc-sz-1", "ehpc-sz-9", "ehpc-bj-1", "ehpc-gone", "ehpc-sz-pbs"} {
		_, _ = targets.For(id)
	}
	if reads != 1 || drv.asked() != 1 {
		t.Fatalf("the listing was read %d times (cloud asked %d times) for five clusters", reads, drv.asked())
	}
	n1, ok1 := targets.GPUNodes("ehpc-sz-1")
	n9, ok9 := targets.GPUNodes("ehpc-sz-9")
	if n1 != 1 || !ok1 || n9 != 0 || ok9 {
		t.Fatalf("GPU nodes: %d %v, %d %v (a cluster no sync listed is unknown, not zero)", n1, ok1, n9, ok9)
	}
	// Synced clusters alone never ask for the listing.
	none, _ := svc.SlurmApplyTargets(providerID, func() (HPCClusterList, error) { t.Fatal("listing asked"); return HPCClusterList{}, nil })
	if _, err := none.For("ehpc-sz-1"); err != nil {
		t.Fatal(err)
	}
}

// A sync whose E-HPC read failed stores the cluster's nodes with no
// scheduler: that is unknown, so the cloud's listing says it is Slurm, and
// the cluster is not refused as "not a Slurm cluster" (2026-11-18 review).
func TestSlurmApplyTargetAsksTheListingWhenTheSyncedSchedulerIsUnknown(t *testing.T) {
	svc, providerID, drv := newHPCListingService(t, "hpc-target-unknown-scheduler")
	drv.set(func(d *hpcListingDriver) {
		d.instances = append(d.instances, computeNode("i-9", "ehpc-sz-9_gpu000", "ehpc-sz-9", "", 1))
	})
	if _, err := svc.Sync(t.Context(), providerID); err != nil {
		t.Fatal(err)
	}
	target, err := svc.SlurmApplyTarget(t.Context(), providerID, "ehpc-sz-9", time.Minute)
	if err != nil || target.Region != "cn-shenzhen" || len(target.ExpectGPU) != 1 {
		t.Fatalf("target = %+v, %v; want the synced node's region and node, the listing's scheduler", target, err)
	}
}
