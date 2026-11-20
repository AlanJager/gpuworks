package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// hpcListDriver is the applyDriver's account that also lists its HPC clusters
// live: a Slurm cluster with synced nodes, an empty one (no node yet), a PBS
// one and a Slurm one still being created.
type hpcListDriver struct {
	*applyDriver
	mu      sync.Mutex
	listErr error
	result  *provider.HPCClusterList
	release chan struct{} // when set, a listing stays out until it is closed
	lists   atomic.Int32
}

func (d *hpcListDriver) set(f func(*hpcListDriver)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(d)
}

func (d *hpcListDriver) Name() string { return "api-hpc-list-provider" }

func (d *hpcListDriver) ListHPCClusters(ctx context.Context, _ provider.Credentials) (provider.HPCClusterList, error) {
	d.lists.Add(1)
	d.mu.Lock()
	listErr, result, release := d.listErr, d.result, d.release
	d.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return provider.HPCClusterList{}, ctx.Err()
		}
	}
	if listErr != nil {
		return provider.HPCClusterList{}, listErr
	}
	if result != nil {
		return *result, nil
	}
	return provider.HPCClusterList{Clusters: []provider.HPCClusterListing{
		{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-1", Name: "slurm-lab", Scheduler: "slurm", SchedulerVersion: "22.05.8"}, Region: "cn-shenzhen", State: "running", Running: true},
		{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-9", Name: "empty", Scheduler: "slurm", SchedulerVersion: "22.05.8"}, Region: "cn-shenzhen", State: "running", Running: true},
		{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-2", Name: "pbs-lab", Scheduler: "pbs", SchedulerVersion: "19.1.1"}, Region: "cn-shenzhen", State: "running", Running: true},
		{HPCCluster: provider.HPCCluster{ID: "ehpc-bj-3", Scheduler: "slurm"}, Region: "cn-beijing", State: "creating"},
	}}, nil
}

func newHPCListRig(t *testing.T) (*applyRig, *hpcListDriver) {
	t.Helper()
	drv := &hpcListDriver{applyDriver: &applyDriver{answer: okRun("")}}
	rig := newApplyRigFor(t, drv.applyDriver, drv)
	return rig, drv
}

// hpcClusterList reads the account's listing fresh (fresh=1, as after a sync):
// the tests change what the cloud answers between reads. Each answer says
// when it was read and that nothing is being read behind it; those two keys
// are checked here and taken out.
func hpcClusterList(t *testing.T, rig *applyRig) map[string]any {
	t.Helper()
	rec := rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/providers/%d/hpc-clusters?fresh=1", rig.providerID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("hpc-clusters = %d %s", rec.Code, rec.Body)
	}
	body := decodeMap(t, rec.Body.Bytes())
	if at, _ := body["listed_at"].(string); at == "" || body["refreshing"] != false {
		t.Fatalf("a fresh listing = %v; want listed_at and refreshing false", body)
	}
	delete(body, "listed_at")
	delete(body, "refreshing")
	return body
}

func hpcClusters(t *testing.T, rig *applyRig) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, it := range hpcClusterList(t, rig)["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

// The listing is the cloud's, not the synced instances': the empty cluster is
// there, each cluster says its facts, and a Slurm cluster that runs and has no
// entry yet is connectable (and the platform can set it up) while a PBS or a
// creating one is not.
func TestProviderHPCClustersListEveryClusterWithWhatConnectingIt(t *testing.T) {
	rig, drv := newHPCListRig(t)
	body := hpcClusterList(t, rig)
	if body["access"] != "ok" || body["partial"] != false || !reflect.DeepEqual(body["failed_regions"], []any{}) || body["total"] != float64(4) {
		t.Fatalf("list envelope = %v", body)
	}
	// One read of the cloud answers the list and every cluster's can_apply.
	if drv.lists.Load() != 1 {
		t.Fatalf("the cloud was read %d times for one request", drv.lists.Load())
	}
	got := hpcClusters(t, rig)
	byID := map[string]map[string]any{}
	for _, c := range got {
		byID[c["external_id"].(string)] = c
	}
	if len(got) != 4 {
		t.Fatalf("clusters = %v", got)
	}
	want := map[string]any{
		"external_id": "ehpc-sz-1", "name": "slurm-lab", "region": "cn-shenzhen", "scheduler": "slurm", "scheduler_version": "22.05.8",
		"state": "running", "state_tone": "success", "vendor_state": "running", "running": true, "gpu_nodes": float64(2), "connectable": true, "can_apply": true,
	}
	if !reflect.DeepEqual(byID["ehpc-sz-1"], want) {
		t.Fatalf("ehpc-sz-1\n got %v\nwant %v", byID["ehpc-sz-1"], want)
	}
	// No node synced yet: the count is unknown (absent, not 0), still
	// connectable, and the platform can place it by the listing.
	if e := byID["ehpc-sz-9"]; e["gpu_nodes"] != nil || e["connectable"] != true || e["can_apply"] != true {
		t.Fatalf("empty cluster = %v", e)
	}
	if p := byID["ehpc-sz-2"]; p["connectable"] != false || p["can_apply"] != false || p["scheduler"] != "pbs" || p["unsupported_reason"] != "scheduler_not_slurm" {
		t.Fatalf("pbs cluster = %v", p)
	}
	// Not running: connectable later; a cluster the vendor gave no name shows its ID.
	if _, has := byID["ehpc-sz-1"]["unsupported_reason"]; has {
		t.Fatalf("a Slurm cluster is called unsupported: %v", byID["ehpc-sz-1"])
	}
	if c := byID["ehpc-bj-3"]; c["connectable"] != false || c["unsupported_reason"] != nil || c["name"] != "ehpc-bj-3" || c["state"] != "creating" || c["state_tone"] != "info" || c["vendor_state"] != "creating" || c["running"] != false || c["not_connectable_reason"] != "cluster_starting" {
		t.Fatalf("creating cluster = %v", c)
	}

	// Once an entry exists the cluster says which, and is no longer connectable.
	uuid, _ := rig.create("ehpc-sz-9")
	for _, c := range hpcClusters(t, rig) {
		if c["external_id"] != "ehpc-sz-9" {
			if _, has := c["entry"]; has {
				t.Fatalf("another cluster has an entry: %v", c)
			}
			continue
		}
		if c["connectable"] != false || !reflect.DeepEqual(c["entry"], map[string]any{"uuid": uuid, "name": "ehpc-ehpc-sz-9"}) {
			t.Fatalf("connected cluster = %v", c)
		}
	}
}

func TestProviderHPCClustersAnswerEachFailureAsItsOwnCode(t *testing.T) {
	rig, drv := newHPCListRig(t)
	path := fmt.Sprintf("/api/v1/providers/%d/hpc-clusters", rig.providerID)
	// Authentication and routing.
	if rec := rig.do("", http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}
	if rec := rig.do(rig.session, http.MethodGet, "/api/v1/providers/9999/hpc-clusters", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown account = %d %s", rec.Code, rec.Body)
	}
	if rec := rig.do(rig.session, http.MethodPost, path, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
	// Any failure of the cloud that left nothing to show.
	drv.set(func(d *hpcListDriver) { d.listErr = errors.New("list E-HPC clusters: region cn-beijing: boom") })
	if rec := rig.do(rig.session, http.MethodGet, path, ""); rec.Code != statusProviderFailed {
		t.Fatalf("cloud failure = %d %s", rec.Code, rec.Body)
	}
}

// A key that may not list E-HPC, an account that never activated it and a
// region that failed while others answered are no errors: the answer is 200
// and says which, so the console shows a quiet notice or a warning and not a
// failure of the whole account.
func TestProviderHPCClustersSayAccessAndPartialInTheAnswer(t *testing.T) {
	rig, drv := newHPCListRig(t)
	drv.set(func(d *hpcListDriver) {
		d.result = &provider.HPCClusterList{Access: provider.HPCAccessPermissionMissing, Permission: "ehpc:ListClusters"}
	})
	body := hpcClusterList(t, rig)
	want := map[string]any{"items": []any{}, "total": float64(0), "access": "permission_missing", "permission": "ehpc:ListClusters", "partial": false, "failed_regions": []any{}}
	if !reflect.DeepEqual(body, want) {
		t.Fatalf("permission missing\n got %v\nwant %v", body, want)
	}

	drv.set(func(d *hpcListDriver) {
		d.result = &provider.HPCClusterList{Access: provider.HPCAccessNotActivated, Code: "ServiceNotActivated"}
	})
	want = map[string]any{"items": []any{}, "total": float64(0), "access": "not_activated", "code": "ServiceNotActivated", "partial": false, "failed_regions": []any{}}
	if body = hpcClusterList(t, rig); !reflect.DeepEqual(body, want) {
		t.Fatalf("not activated\n got %v\nwant %v", body, want)
	}

	drv.set(func(d *hpcListDriver) {
		d.result = &provider.HPCClusterList{
			Clusters: []provider.HPCClusterListing{{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-1", Name: "slurm-lab", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true}},
			Failed: []provider.HPCRegionFailure{
				{Region: "cn-beijing", Code: "Throttling.User", Message: "flow control"},
				{Region: "cn-hangzhou", Code: "Forbidden.RAM", Message: "not authorized", Permission: "ehpc:ListClusters"},
			},
		}
	})
	body = hpcClusterList(t, rig)
	if body["access"] != "ok" || body["partial"] != true || body["total"] != float64(1) {
		t.Fatalf("partial = %v", body)
	}
	wantFailed := []any{
		map[string]any{"region": "cn-beijing", "code": "Throttling.User", "message": "flow control"},
		map[string]any{"region": "cn-hangzhou", "code": "Forbidden.RAM", "message": "not authorized", "permission": "ehpc:ListClusters"},
	}
	if !reflect.DeepEqual(body["failed_regions"], wantFailed) {
		t.Fatalf("failed_regions = %v", body["failed_regions"])
	}
}

// A kind whose driver lists no HPC clusters answers 501 like the managed
// Kubernetes listing does.
func TestProviderHPCClustersOfAKindWithoutThemIs501(t *testing.T) {
	drv := &plainDriver{}
	_ = drv
	rig := newApplyRig(t, okRun("")) // applyDriver is no HPCClusterProvider
	rec := rig.do(rig.session, http.MethodGet, fmt.Sprintf("/api/v1/providers/%d/hpc-clusters", rig.providerID), "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
}

// An entry says whether the platform can operate on it and, when not, why:
// the reasons the apply refusal also gives, and a revoked one still removes.
func TestSlurmEntryOperationBlockedSaysWhy(t *testing.T) {
	rig, _ := newHPCListRig(t)
	entry := func(cluster string) map[string]any {
		_, e := rig.create(cluster)
		return e
	}
	ok := entry("ehpc-sz-1")
	if ok["can_apply"] != true || ok["can_remove"] != true || ok["operation_blocked"] != nil {
		t.Fatalf("applicable entry = %v", ok)
	}
	// Placed by the live listing though no node of it was synced.
	if e := entry("ehpc-sz-9"); e["can_apply"] != true || e["operation_blocked"] != nil {
		t.Fatalf("entry of an empty cluster = %v", e)
	}
	for cluster, reason := range map[string]string{"ehpc-sz-2": "not_slurm_cluster", "ehpc-gone": "cluster_unknown"} {
		if e := entry(cluster); e["can_apply"] != false || e["can_remove"] != false || e["operation_blocked"] != reason {
			t.Fatalf("%s: %v, want blocked %s", cluster, e, reason)
		}
	}
	uuid := ok["uuid"].(string)
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters/"+uuid+"/revoke", "")
	revoked := decodeMap(t, rec.Body.Bytes())
	if revoked["can_apply"] != false || revoked["can_remove"] != true || revoked["operation_blocked"] != "revoked" {
		t.Fatalf("revoked entry = %v", revoked)
	}
	_, manual, _ := createSlurm(t, rig.tokenRig, slurmCreateBody)
	_ = manual
	for _, it := range decodeMap(t, rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "").Body.Bytes())["items"].([]any) {
		if m := it.(map[string]any); m["name"] == "hpc" && (m["can_apply"] != false || m["can_remove"] != false || m["operation_blocked"] != "no_source") {
			t.Fatalf("an entry made by hand = %v", m)
		}
	}
}

// A listing that fails, or is not read yet, is a reason of its own on the
// entry, never "the cloud does not have this cluster", and a entry whose
// cluster has synced nodes does not depend on the listing at all.
func TestSlurmEntryOperationBlockedSaysTheListingFailedOrIsNotReadYet(t *testing.T) {
	t.Run("failed", func(t *testing.T) {
		rig, drv := newHPCListRig(t)
		drv.set(func(d *hpcListDriver) { d.listErr = errors.New("cloud down") })
		if e := func() map[string]any { _, e := rig.create("ehpc-sz-9"); return e }(); e["can_apply"] != false || e["operation_blocked"] != "cluster_listing_failed" {
			t.Fatalf("entry = %v", e)
		}
		if _, e := rig.create("ehpc-sz-1"); e["can_apply"] != true || e["operation_blocked"] != nil {
			t.Fatalf("a cluster with synced nodes needs no listing: %v", e)
		}
	})
	t.Run("not read yet, then read", func(t *testing.T) {
		defer func(old time.Duration) { slurmCloudListWait = old }(slurmCloudListWait)
		slurmCloudListWait = 30 * time.Millisecond
		rig, drv := newHPCListRig(t)
		release := make(chan struct{})
		drv.set(func(d *hpcListDriver) { d.release = release })
		uuid, e := rig.create("ehpc-sz-9")
		if e["can_apply"] != false || e["operation_blocked"] != "cluster_listing_pending" {
			t.Fatalf("entry = %v", e)
		}
		// The slow read went on without the request; the polled list finds its answer.
		close(release)
		deadline := time.Now().Add(5 * time.Second)
		for {
			rec := rig.do(rig.session, http.MethodGet, "/api/v1/slurm/clusters", "")
			var found map[string]any
			for _, it := range decodeMap(t, rec.Body.Bytes())["items"].([]any) {
				if m := it.(map[string]any); m["uuid"] == uuid {
					found = m
				}
			}
			if found["can_apply"] == true {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the entry never became applicable: %v", found)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if drv.lists.Load() != 1 {
			t.Fatalf("the cloud was read %d times", drv.lists.Load())
		}
	})
}

// The state is the server's code with a tone, whatever the vendor's spelling,
// and a Slurm cluster with no entry that cannot be connected because of its
// state says why; the scheduler's reason wins where both apply, and a cluster
// that does not name its scheduler yet is not called a non-Slurm one.
func TestProviderHPCClustersStateCodesToneAndWhyNotConnectable(t *testing.T) {
	rig, drv := newHPCListRig(t)
	listing := func(id, scheduler, state string) provider.HPCClusterListing {
		return provider.HPCClusterListing{HPCCluster: provider.HPCCluster{ID: id, Name: id, Scheduler: scheduler}, Region: "cn-hangzhou", State: state, Running: state == "running"}
	}
	drv.set(func(d *hpcListDriver) {
		d.result = &provider.HPCClusterList{Clusters: []provider.HPCClusterListing{
			listing("c-run", "slurm", "running"), listing("c-init", "slurm", "initing"), listing("c-exc", "slurm", "exception"),
			listing("c-stop", "slurm", "stopped"), listing("c-rel", "slurm", "releasing"), listing("c-pend", "slurm", "pending"),
			listing("c-new", "", "creating"), listing("c-pbs-exc", "pbs", "exception"), listing("c-odd", "slurm", "Upgrading"),
		}}
	})
	byID := map[string]map[string]any{}
	for _, c := range hpcClusters(t, rig) {
		byID[c["external_id"].(string)] = c
	}
	for id, want := range map[string][4]any{
		// state, tone, not_connectable_reason, unsupported_reason
		"c-run":     {"running", "success", nil, nil},
		"c-init":    {"initing", "info", "cluster_starting", nil},
		"c-exc":     {"exception", "danger", "cluster_exception", nil},
		"c-stop":    {"stopped", "neutral", "cluster_stopped", nil},
		"c-rel":     {"releasing", "neutral", "cluster_releasing", nil},
		"c-pend":    {"pending", "neutral", "cluster_pending", nil},
		"c-new":     {"creating", "info", "cluster_starting", nil},
		"c-pbs-exc": {"exception", "danger", nil, "scheduler_not_slurm"},
		"c-odd":     {"unknown", "neutral", "cluster_state_unknown", nil},
	} {
		c := byID[id]
		if c["state"] != want[0] || c["state_tone"] != want[1] || c["not_connectable_reason"] != want[2] || c["unsupported_reason"] != want[3] {
			t.Errorf("%s = %v, want %v", id, c, want)
		}
	}
	if byID["c-odd"]["vendor_state"] != "Upgrading" || byID["c-exc"]["connectable"] != false || byID["c-run"]["connectable"] != true {
		t.Errorf("odd = %v", byID["c-odd"])
	}
}

// The cloud's live count of an account's cluster wins over the account's last
// instance sync, which can be old: a cluster the sync saw with 2 GPU nodes that
// now lists 3 compute nodes says 3 compute nodes, not 2 GPU nodes. A cluster
// whose listing carries no count falls back to the synced one, and the live
// count is not called a GPU count (it has its own field).
func TestProviderHPCClustersPreferTheLiveNodeCountToTheSyncedOne(t *testing.T) {
	rig, drv := newHPCListRig(t)
	drv.set(func(d *hpcListDriver) {
		d.result = &provider.HPCClusterList{Clusters: []provider.HPCClusterListing{
			{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-1", Name: "grown", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true, ComputeNodes: intp(3)},
			{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-9", Name: "emptied", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true, ComputeNodes: intp(0)},
		}}
	})
	byID := map[string]map[string]any{}
	for _, c := range hpcClusters(t, rig) {
		byID[c["external_id"].(string)] = c
	}
	if c := byID["ehpc-sz-1"]; c["compute_nodes"] != float64(3) || c["gpu_nodes"] != nil {
		t.Fatalf("a cluster that grew since the sync = %v", c)
	}
	// Zero is a count: the cloud says there are no nodes, not that it does not know.
	if c := byID["ehpc-sz-9"]; c["compute_nodes"] != float64(0) || c["gpu_nodes"] != nil {
		t.Fatalf("a cluster the cloud lists as empty = %v", c)
	}
	// The cloud gave no count: the synced one (2 nodes with a card) answers.
	drv.set(func(d *hpcListDriver) { d.result = nil })
	for _, c := range hpcClusters(t, rig) {
		if c["external_id"] == "ehpc-sz-1" && (c["gpu_nodes"] != float64(2) || c["compute_nodes"] != nil) {
			t.Fatalf("a listing without a count = %v", c)
		}
	}
}

// Without fresh, a page gets the last listing at once: the cloud changed
// within the refresh window, and the answer is still the one read before, with
// the time it was read.
func TestProviderHPCClustersAnswerTheLastListingWithoutFresh(t *testing.T) {
	rig, drv := newHPCListRig(t)
	drv.set(func(d *hpcListDriver) {
		d.result = &provider.HPCClusterList{Clusters: []provider.HPCClusterListing{
			{HPCCluster: provider.HPCCluster{ID: "ehpc-sz-1", Name: "first", Scheduler: "slurm"}, Region: "cn-shenzhen", State: "running", Running: true},
		}}
	})
	path := fmt.Sprintf("/api/v1/providers/%d/hpc-clusters", rig.providerID)
	first := decodeMap(t, rig.do(rig.session, http.MethodGet, path, "").Body.Bytes())
	drv.set(func(d *hpcListDriver) { d.result = &provider.HPCClusterList{} })
	again := decodeMap(t, rig.do(rig.session, http.MethodGet, path, "").Body.Bytes())
	if again["total"] != float64(1) || again["listed_at"] != first["listed_at"] || again["refreshing"] != false {
		t.Fatalf("second read = %v; want the first listing (%v)", again, first["listed_at"])
	}
	if fresh := hpcClusterList(t, rig); fresh["total"] != float64(0) {
		t.Fatalf("fresh read = %v; want the cloud's new answer", fresh)
	}
}
