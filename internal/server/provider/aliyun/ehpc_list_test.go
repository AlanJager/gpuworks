package aliyun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// hpcListFake is an account with E-HPC clusters in two regions and none in a
// third, which has no E-HPC endpoint.
func hpcListFake(t *testing.T, answer func(c fakeCall, region string) (int, any)) (*Driver, *fakeAliyun) {
	return newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shenzhen", "cn-beijing", "ap-southeast-1")
		case "ListRegions":
			return 200, ehpcRegionsBody("", "cn-shenzhen", "cn-beijing", "ap-southeast-1")
		case "ListClusters":
			return answer(c, c.query["RegionId"])
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	})
}

// Every region is asked, and a cluster is listed whether or not any of its
// nodes is an instance the account lists: this is what lets a cluster with
// no GPU node yet be connected.
func TestListHPCClustersListsEveryRegionsClusters(t *testing.T) {
	d, fake := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		if c.host != "ehpc."+region+".aliyuncs.com" || c.query["PageSize"] != "50" {
			t.Errorf("ListClusters call = %+v", c)
		}
		switch region {
		case "cn-shenzhen":
			stopped := ehpcClusterJSON("ehpc-sz-2", "pbs-lab", "PBS", "19.1.1")
			stopped["ClusterStatus"] = "creating"
			return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-sz-1", "slurm-lab", "slurm", "22.05.8"), stopped)
		case "cn-beijing":
			return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-bj-1", "bj", "Slurm", "23.02.7"))
		}
		return 200, ehpcClustersBody()
	})
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil || list.Access != "" || list.Partial() {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got := list.Clusters
	byID := map[string]provider.HPCClusterListing{}
	for _, c := range got {
		byID[c.ID] = c
	}
	one := 1 // the fixture's clusters each count one running compute node
	want := map[string]provider.HPCClusterListing{
		"ehpc-sz-1": {HPCCluster: provider.HPCCluster{ID: "ehpc-sz-1", Name: "slurm-lab", Scheduler: "slurm", SchedulerVersion: "22.05.8"}, Region: "cn-shenzhen", State: "running", Running: true, ComputeNodes: &one},
		"ehpc-sz-2": {HPCCluster: provider.HPCCluster{ID: "ehpc-sz-2", Name: "pbs-lab", Scheduler: "pbs", SchedulerVersion: "19.1.1"}, Region: "cn-shenzhen", State: "creating", ComputeNodes: &one},
		"ehpc-bj-1": {HPCCluster: provider.HPCCluster{ID: "ehpc-bj-1", Name: "bj", Scheduler: "slurm", SchedulerVersion: "23.02.7"}, Region: "cn-beijing", State: "running", Running: true, ComputeNodes: &one},
	}
	if len(got) != len(want) {
		t.Fatalf("listed %d clusters: %+v", len(got), got)
	}
	for id, w := range want {
		if !reflect.DeepEqual(byID[id], w) {
			t.Errorf("%s = %+v, want %+v", id, byID[id], w)
		}
	}
	if n := strings.Count(strings.Join(fake.actions(), ","), "ListClusters"); n != 3 {
		t.Fatalf("ListClusters called %d times, want once per region", n)
	}
}

// The listing says how many compute nodes a cluster has, in any state, and
// says nothing when the answer does not carry every count (which is not zero).
func TestListHPCClustersCountsTheComputeNodesOfACluster(t *testing.T) {
	counts := func(id string, nodes any) map[string]any {
		c := ehpcClusterJSON(id, id, "slurm", "22.05.8")
		if nodes == nil {
			delete(c, "Nodes")
		} else {
			c["Nodes"] = nodes
		}
		return c
	}
	d, _ := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		if region != "cn-shenzhen" {
			return 200, ehpcClustersBody()
		}
		return 200, ehpcClustersBody(
			counts("none", map[string]any{"RunningCounts": 0, "AbnormalCounts": 0, "CreatingCounts": 0}),
			counts("mixed", map[string]any{"RunningCounts": 2, "AbnormalCounts": 1, "CreatingCounts": 3}),
			counts("creating", map[string]any{"RunningCounts": 0, "AbnormalCounts": 0, "CreatingCounts": 1}),
			counts("partial", map[string]any{"RunningCounts": 0}),
			counts("missing", nil),
		)
	})
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range list.Clusters {
		got[c.ID] = "unknown"
		if c.ComputeNodes != nil {
			got[c.ID] = fmt.Sprint(*c.ComputeNodes)
		}
	}
	want := map[string]string{"none": "0", "mixed": "6", "creating": "1", "partial": "unknown", "missing": "unknown"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("compute nodes = %v, want %v", got, want)
	}
}

// A region with more clusters than a page is read to its end.
func TestListHPCClustersPagesAndReadsEveryPage(t *testing.T) {
	d, _ := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		if region != "cn-shenzhen" {
			return 200, ehpcClustersBody()
		}
		var page []map[string]any
		if c.query["PageNumber"] == "1" {
			for i := 0; i < ehpcPageSize; i++ {
				page = append(page, ehpcClusterJSON(fmt.Sprintf("ehpc-sz-%02d", i), "c", "slurm", "22.05.8"))
			}
		} else {
			page = append(page, ehpcClusterJSON("ehpc-sz-last", "late", "slurm", "22.05.8"))
		}
		body := ehpcClustersBody(page...)
		body["TotalCount"] = ehpcPageSize + 1
		body["PageNumber"] = 1
		return 200, body
	})
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if got := list.Clusters; err != nil || len(got) != ehpcPageSize+1 {
		t.Fatalf("listed %d clusters, %v; want %d", len(got), err, ehpcPageSize+1)
	}
}

// ehpcDenied is how a RAM user without ehpc:ListClusters is refused, in both
// spellings the cloud uses.
func ehpcDenied(spelling int) (int, any) {
	if spelling == 0 {
		return 403, map[string]string{"Code": "NoPermission", "Message": "You are not authorized to perform this operation. Action: ehpc:ListClusters", "RequestId": "req-ram"}
	}
	return 403, map[string]string{"Code": "Forbidden.RAM", "Message": "User not authorized to operate on the specified resource, or this API doesn't support RAM.", "RequestId": "req-ram", "HostId": "ehpc.cn-shenzhen.aliyuncs.com"}
}

// A key that may not list E-HPC clusters in any region is not an error: the
// account has no clusters to show, and says which RAM action it lacks.
func TestListHPCClustersNamesTheMissingPermissionAsAnAccessNotAnError(t *testing.T) {
	for spelling := 0; spelling < 2; spelling++ {
		d, _ := hpcListFake(t, func(fakeCall, string) (int, any) { return ehpcDenied(spelling) })
		list, err := d.ListHPCClusters(context.Background(), userCreds)
		if err != nil || list.Access != provider.HPCAccessPermissionMissing || list.Permission != "ehpc:ListClusters" || len(list.Clusters) != 0 || list.Partial() {
			t.Fatalf("spelling %d: list = %+v, %v", spelling, list, err)
		}
	}
}

// An account that never activated E-HPC has no clusters, quietly: its regions
// answer with the product's not-activated family of codes, kept as Code.
func TestListHPCClustersAnAccountThatNeverActivatedEHPCHasNone(t *testing.T) {
	for _, code := range []string{"ServiceNotActivated", "Forbidden.AccountNotActivated", "InvalidAccountStatus.NotActivated", "Service.NotOpen"} {
		d, _ := hpcListFake(t, func(fakeCall, string) (int, any) {
			return 403, map[string]string{"Code": code, "Message": "The service is not activated for your account.", "RequestId": "req-off"}
		})
		list, err := d.ListHPCClusters(context.Background(), userCreds)
		if err != nil || list.Access != provider.HPCAccessNotActivated || list.Code != code || len(list.Clusters) != 0 || list.Partial() {
			t.Fatalf("%s: list = %+v, %v", code, list, err)
		}
	}
	// Not activated in some regions only: the others' clusters are the list, with nothing failed.
	d, _ := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		if region == "cn-shenzhen" {
			return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-sz-1", "slurm-lab", "slurm", "22.05.8"))
		}
		return 403, map[string]string{"Code": "ServiceNotActivated", "Message": "off", "RequestId": "req-off"}
	})
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil || list.Access != "" || list.Partial() || len(list.Clusters) != 1 {
		t.Fatalf("partly activated: %+v, %v", list, err)
	}
}

// A region the product is not available in is skipped.
func TestListHPCClustersSkipsRegionsWithoutTheService(t *testing.T) {
	d, _ := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		switch region {
		case "ap-southeast-1":
			return 400, map[string]string{"Code": "InvalidRegionId.NotFound", "Message": "The specified RegionId does not exist.", "RequestId": "req-region"}
		case "cn-shenzhen":
			return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-sz-1", "slurm-lab", "slurm", "22.05.8"))
		}
		return 200, ehpcClustersBody()
	})
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil || len(list.Clusters) != 1 || list.Clusters[0].ID != "ehpc-sz-1" || list.Partial() {
		t.Fatalf("listed %+v, %v", list, err)
	}
}

// A region that fails while others answer does not take their clusters down:
// the list is partial and names the failed regions with the cloud's word, a
// RAM refusal of one region (a policy limited to some regions) naming its action.
func TestListHPCClustersReturnsTheClustersOfTheRegionsThatAnsweredAndNamesTheOthers(t *testing.T) {
	d, _ := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		switch region {
		case "cn-shenzhen":
			return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-sz-1", "slurm-lab", "slurm", "22.05.8"))
		case "cn-beijing":
			return 400, map[string]string{"Code": "Throttling.User", "Message": "Request was denied due to user flow control.", "RequestId": "req-slow"}
		}
		return ehpcDenied(1)
	})
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Clusters) != 1 || list.Clusters[0].ID != "ehpc-sz-1" || list.Access != "" || !list.Partial() || len(list.Failed) != 2 {
		t.Fatalf("list = %+v", list)
	}
	byRegion := map[string]provider.HPCRegionFailure{}
	for _, f := range list.Failed {
		byRegion[f.Region] = f
	}
	if f := byRegion["cn-beijing"]; f.Code != "Throttling.User" || f.Permission != "" || !strings.Contains(f.Message, "flow control") {
		t.Errorf("cn-beijing failure = %+v", f)
	}
	if f := byRegion["ap-southeast-1"]; f.Code != "Forbidden.RAM" || f.Permission != "ehpc:ListClusters" {
		t.Errorf("ap-southeast-1 failure = %+v", f)
	}
}

// When no region answered and some failed for another reason than RAM or
// activation, there is nothing to show and the failure is the listing's.
func TestListHPCClustersFailsWhenNoRegionAnsweredAndOneFailedOtherwise(t *testing.T) {
	d, _ := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		if region == "cn-beijing" {
			return 400, map[string]string{"Code": "InvalidParameter", "Message": "bad", "RequestId": "req-bad"}
		}
		return ehpcDenied(0)
	})
	if _, err := d.ListHPCClusters(context.Background(), userCreds); err == nil || !strings.Contains(err.Error(), "cn-beijing") || !strings.Contains(err.Error(), "InvalidParameter") {
		t.Fatalf("err = %v, want the failing region and the cloud's word", err)
	}
}

func TestEHPCNotActivatedIsTheNotActivatedFamilyOnly(t *testing.T) {
	for code, want := range map[string]bool{
		"ServiceNotActivated": true, "Forbidden.AccountNotActivated": true, "Service.NotOpen": true,
		"NoPermission": false, "Forbidden.RAM": false, "Throttling.User": false, "InvalidParameter": false,
	} {
		if got := ehpcNotActivated(&apiError{Code: code}) != ""; got != want {
			t.Errorf("ehpcNotActivated(%s) = %v, want %v", code, got, want)
		}
	}
	if ehpcNotActivated(errors.New("boom")) != "" {
		t.Error("a network failure is not an activation state")
	}
}

func TestNoEHPCEndpointIsADNSMissOrAnInvalidRegionOnly(t *testing.T) {
	missing := fmt.Errorf("Post: %w", &net.DNSError{Err: "no such host", Name: "ehpc.x.aliyuncs.com", IsNotFound: true})
	timeout := fmt.Errorf("Post: %w", &net.DNSError{Err: "i/o timeout", Name: "ehpc.x.aliyuncs.com", IsTimeout: true})
	for err, want := range map[error]bool{
		missing: true, timeout: false, errors.New("boom"): false,
		&apiError{Code: "InvalidRegionId.NotFound"}: true, &apiError{Code: "Throttling.User"}: false,
	} {
		if got := noEHPCEndpoint(err); got != want {
			t.Errorf("noEHPCEndpoint(%v) = %v, want %v", err, got, want)
		}
	}
}

// ehpcRegionsBody is shaped like E-HPC's ListRegions response (2024-07-30):
// Regions with RegionId and LocalName, and a NextToken that is empty on the
// last page.
func ehpcRegionsBody(next string, ids ...string) map[string]any {
	regions := []map[string]string{}
	for _, id := range ids {
		regions = append(regions, map[string]string{"RegionId": id, "LocalName": id})
	}
	return map[string]any{"RequestId": "req-ehpc-regions", "TotalCount": len(ids), "MaxResults": 100, "NextToken": next, "Regions": regions}
}

// The account has one cluster, in Hangzhou. E-HPC is offered in two regions
// only; the central endpoint answers ListClusters for any region with the
// Hangzhou cluster (the response has no region), and the regional host of a
// region without the service is unreachable. The cluster is listed once, in
// Hangzhou, E-HPC is asked for its regions rather than every ECS region, and
// no region is asked on the central endpoint.
func TestListHPCClustersListsAClusterOnceDespiteTheCentralEndpointAnsweringEveryRegion(t *testing.T) {
	var listed []fakeCall
	var mu sync.Mutex
	d, fake := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-hangzhou", "cn-shenzhen", "eu-west-1", "ap-southeast-1", "us-east-1", "cn-nanjing")
		case "ListRegions":
			if c.host != "ehpc.cn-hangzhou.aliyuncs.com" || c.query["SpecCode"] != "Next" || c.query["RegionId"] != "" {
				t.Errorf("ListRegions call = %+v", c)
			}
			return 200, ehpcRegionsBody("", "cn-shenzhen", "cn-hangzhou")
		case "ListClusters":
			mu.Lock()
			listed = append(listed, c)
			mu.Unlock()
			if c.host == "ehpc.cn-hangzhou.aliyuncs.com" {
				// The central host's answer does not depend on RegionId.
				return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-hz-jhiq7ivhvo", "hz-lab", "slurm", "22.05.8"))
			}
			return 200, ehpcClustersBody()
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	}, "ehpc.eu-west-1.aliyuncs.com", "ehpc.ap-southeast-1.aliyuncs.com", "ehpc.us-east-1.aliyuncs.com", "ehpc.cn-nanjing.aliyuncs.com")
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil || list.Access != "" || list.Partial() {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if len(list.Clusters) != 1 || list.Clusters[0].ID != "ehpc-hz-jhiq7ivhvo" || list.Clusters[0].Region != "cn-hangzhou" {
		t.Fatalf("clusters = %+v; want the Hangzhou cluster once, in cn-hangzhou", list.Clusters)
	}
	regions := map[string]bool{}
	for _, c := range listed {
		if c.host != "ehpc."+c.query["RegionId"]+".aliyuncs.com" {
			t.Errorf("ListClusters for %s went to %s", c.query["RegionId"], c.host)
		}
		regions[c.query["RegionId"]] = true
	}
	if len(regions) != 2 || !regions["cn-hangzhou"] || !regions["cn-shenzhen"] {
		t.Fatalf("ListClusters asked %v; want only the regions E-HPC is offered in", regions)
	}
	if strings.Contains(strings.Join(fake.actions(), ","), "DescribeRegions") {
		t.Errorf("every ECS region was listed: %v", fake.actions())
	}
}

// ListRegions is read to its last page, and the regions the account selected
// limit the ones asked.
func TestListHPCClustersAsksTheRegionsEHPCListsWithinTheAccountsSelection(t *testing.T) {
	var tokens []string
	var asked sync.Map
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-shenzhen", "cn-beijing", "cn-hangzhou", "cn-chengdu")
		case "ListRegions":
			tokens = append(tokens, c.query["NextToken"])
			if c.query["NextToken"] == "" {
				return 200, ehpcRegionsBody("page2", "cn-shenzhen", "cn-beijing")
			}
			return 200, ehpcRegionsBody("", "cn-hangzhou")
		case "ListClusters":
			asked.Store(c.query["RegionId"], true)
			return 200, ehpcClustersBody()
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	})
	count := func() []string {
		var out []string
		asked.Range(func(k, _ any) bool { out = append(out, k.(string)); return true })
		sort.Strings(out)
		return out
	}
	if _, err := d.ListHPCClusters(context.Background(), userCreds); err != nil {
		t.Fatal(err)
	}
	if got := count(); !reflect.DeepEqual(got, []string{"cn-beijing", "cn-hangzhou", "cn-shenzhen"}) || !reflect.DeepEqual(tokens, []string{"", "page2"}) {
		t.Fatalf("asked %v with tokens %q; want the three regions from both pages of ListRegions (not cn-chengdu)", got, tokens)
	}
	asked = sync.Map{}
	chosen := userCreds
	chosen.Extra = map[string]string{"regions": "cn-hangzhou, cn-chengdu"}
	if _, err := d.ListHPCClusters(context.Background(), chosen); err != nil {
		t.Fatal(err)
	}
	if got := count(); !reflect.DeepEqual(got, []string{"cn-hangzhou"}) {
		t.Fatalf("asked %v; want only the selected region E-HPC is offered in", got)
	}
}

// When E-HPC cannot say where it is offered (the key may not call
// ListRegions), every region in scope is asked on its own endpoint and
// nothing falls back to the central one: a region without the service does not
// borrow the Hangzhou cluster, and is reported as one that could not be read.
func TestListHPCClustersWithoutListRegionsAsksEachRegionOnItsOwnEndpointOnly(t *testing.T) {
	var listed []fakeCall
	var mu sync.Mutex
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "DescribeRegions":
			return 200, regionsBody("cn-hangzhou", "eu-west-1")
		case "ListRegions":
			return 403, map[string]string{"Code": "NoPermission", "Message": "You are not authorized to perform this operation. Action: ehpc:ListRegions", "RequestId": "req-ram"}
		case "ListClusters":
			mu.Lock()
			listed = append(listed, c)
			mu.Unlock()
			if c.host == "ehpc.cn-hangzhou.aliyuncs.com" {
				return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-hz-jhiq7ivhvo", "hz-lab", "slurm", "22.05.8"))
			}
			return 200, ehpcClustersBody()
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	}, "ehpc.eu-west-1.aliyuncs.com")
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Clusters) != 1 || list.Clusters[0].Region != "cn-hangzhou" || !list.Partial() || list.Failed[0].Region != "eu-west-1" {
		t.Fatalf("list = %+v", list)
	}
	for _, c := range listed {
		if c.query["RegionId"] == "eu-west-1" {
			t.Errorf("eu-west-1 was asked at %s: the central endpoint must not answer for it", c.host)
		}
	}
}

// A gateway that ignores the region (an operator's endpoint override) returns
// the same cluster for every region asked: it is listed once, in the first
// region in order.
func TestListHPCClustersListsAClusterOnceWhenSeveralRegionsReturnIt(t *testing.T) {
	d, _ := newFake(t, func(c fakeCall) (int, any) {
		switch c.action {
		case "ListRegions":
			return 200, ehpcRegionsBody("", "cn-shenzhen", "cn-hangzhou", "ap-southeast-1")
		case "ListClusters":
			return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-hz-jhiq7ivhvo", "hz-lab", "slurm", "22.05.8"))
		}
		return 404, map[string]string{"Code": "InvalidAction.NotFound", "Message": c.action}
	})
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil || len(list.Clusters) != 1 || list.Clusters[0].Region != "ap-southeast-1" {
		t.Fatalf("list = %+v, %v; want the cluster once, in the first region by name", list, err)
	}
}

// A region whose endpoint does not answer fails alone within its own budget:
// the clusters of the regions that answered are listed, and it is a failed
// region (2026-11-18 review: one slow region emptied the whole listing).
func TestListHPCClustersKeepsTheAnsweredRegionsWhenOneDoesNotAnswer(t *testing.T) {
	old := ehpcRegionTimeout
	ehpcRegionTimeout = 100 * time.Millisecond
	t.Cleanup(func() { ehpcRegionTimeout = old })
	d, _ := hpcListFake(t, func(c fakeCall, region string) (int, any) {
		switch region {
		case "cn-shenzhen":
			return 200, ehpcClustersBody(ehpcClusterJSON("ehpc-sz-1", "slurm-lab", "slurm", "22.05.8"))
		case "cn-beijing":
			time.Sleep(2 * time.Second)
			return 200, ehpcClustersBody()
		}
		return ehpcDenied(1)
	})
	start := time.Now()
	list, err := d.ListHPCClusters(context.Background(), userCreds)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("the listing waited %s for the region that does not answer", time.Since(start))
	}
	if len(list.Clusters) != 1 || list.Clusters[0].ID != "ehpc-sz-1" || !list.Partial() {
		t.Fatalf("list = %+v", list)
	}
	failed := false
	for _, f := range list.Failed {
		failed = failed || f.Region == "cn-beijing"
	}
	if !failed {
		t.Fatalf("the region that did not answer is not a failed region: %+v", list.Failed)
	}
}
