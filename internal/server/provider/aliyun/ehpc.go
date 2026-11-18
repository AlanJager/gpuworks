package aliyun

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// E-HPC 2.0 clusters of the account, read through the 2024-07-30 OpenAPI:
// https://help.aliyun.com/zh/e-hpc/e-hpc-next/developer-reference/api-ehpc-2024-07-30-overview
// Their compute nodes are ordinary ECS instances, listed with the others;
// what E-HPC adds is which cluster and scheduler a node is under, because the
// cluster's scheduler owns the node's cards and its queue the node's
// lifecycle (docs/how-to/connect-slurm-nodes.md).

var ehpcProduct = product{name: "ehpc", version: "2024-07-30", host: func(region string) string {
	if region == "" {
		return "ehpc.cn-hangzhou.aliyuncs.com"
	}
	return "ehpc." + region + ".aliyuncs.com"
}}

// ehpcClusterTag is the tag E-HPC puts on its clusters' compute nodes; its
// value is the cluster ID. The manager and login nodes do not carry it.
const ehpcClusterTag = "ehpc.aliyun.com"

// ehpcPageSize is the ListClusters page size.
const ehpcPageSize = 50

type ehpcCluster struct {
	ClusterID     string `json:"ClusterId"`
	ClusterName   string `json:"ClusterName"`
	ClusterStatus string `json:"ClusterStatus"`
	// Nodes counts the cluster's compute nodes by state (ListClusters
	// 2024-07-30 Nodes: RunningCounts, AbnormalCounts, CreatingCounts); nil
	// when the answer carries none, which is not zero.
	Nodes   *ehpcNodeCounts `json:"Nodes"`
	Manager struct {
		Scheduler struct {
			Type    string `json:"Type"`
			Version string `json:"Version"`
		} `json:"Scheduler"`
	} `json:"Manager"`
}

// ehpcNodeCounts are the compute nodes of a cluster by state. A count the
// answer leaves out is absent, not zero: only a listing that gives all three
// says how many nodes there are.
type ehpcNodeCounts struct {
	Running  *int `json:"RunningCounts"`
	Abnormal *int `json:"AbnormalCounts"`
	Creating *int `json:"CreatingCounts"`
}

// total is how many compute nodes the cluster has in any state; nil when a
// count is missing.
func (n *ehpcNodeCounts) total() *int {
	if n == nil || n.Running == nil || n.Abnormal == nil || n.Creating == nil {
		return nil
	}
	sum := *n.Running + *n.Abnormal + *n.Creating
	return &sum
}

// listEHPCClusters pages through the region's clusters (ListClusters). With
// regionalOnly only the region's own E-HPC endpoint answers: the central
// endpoint answers a region without a service of its own with the clusters of
// the central one (Hangzhou), and the response carries no region, so a listing
// that attributes clusters to the region it asked must not accept that.
func (d *Driver) listEHPCClusters(ctx context.Context, creds provider.Credentials, region string, regionalOnly bool) ([]ehpcCluster, error) {
	call := d.api.call
	if regionalOnly {
		call = d.api.callRegional
	}
	var out []ehpcCluster
	for page := 1; page <= 100; page++ {
		var resp struct {
			TotalCount int           `json:"TotalCount"`
			Clusters   []ehpcCluster `json:"Clusters"`
		}
		params := map[string]string{"PageNumber": strconv.Itoa(page), "PageSize": strconv.Itoa(ehpcPageSize)}
		if err := call(ctx, creds, ehpcProduct, region, "ListClusters", params, &resp); err != nil {
			return out, err
		}
		out = append(out, resp.Clusters...)
		if len(resp.Clusters) < ehpcPageSize || len(out) >= resp.TotalCount {
			break
		}
	}
	return out, nil
}

// ehpcClusters describes the clusters of the region's compute nodes, by
// cluster ID, for a region where some instance carries ehpcClusterTag. It is
// a description of instances the listing has anyway, so it never fails the
// listing: when a key may not read E-HPC (ehpc:ListClusters) or the call
// fails, the nodes are still a cluster's (the tag says so) and only the
// cluster's name and scheduler are unknown until a sync that reads them.
func (d *Driver) ehpcClusters(ctx context.Context, creds provider.Credentials, region string) map[string]ehpcCluster {
	out := map[string]ehpcCluster{}
	list, _ := d.listEHPCClusters(ctx, creds, region, false)
	for _, c := range list {
		out[c.ClusterID] = c
	}
	return out
}

// ehpcRegionsPageSize is the ListRegions page size (1 to 100).
const ehpcRegionsPageSize = 100

// listEHPCRegions asks E-HPC itself in which regions it is offered
// (ListRegions, SpecCode Next: E-HPC Cluster, the product ListClusters
// lists), following NextToken to the end. The call goes to the central
// endpoint, which knows every region.
func (d *Driver) listEHPCRegions(ctx context.Context, creds provider.Credentials) ([]string, error) {
	var out []string
	token := ""
	for page := 0; page < 100; page++ {
		var resp struct {
			Regions []struct {
				RegionID string `json:"RegionId"`
			} `json:"Regions"`
			NextToken string `json:"NextToken"`
		}
		params := map[string]string{"SpecCode": "Next", "MaxResults": strconv.Itoa(ehpcRegionsPageSize)}
		if token != "" {
			params["NextToken"] = token
		}
		if err := d.api.callRegional(ctx, creds, ehpcProduct, "", "ListRegions", params, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Regions {
			if r.RegionID != "" {
				out = append(out, r.RegionID)
			}
		}
		if resp.NextToken == "" {
			break
		}
		token = resp.NextToken
	}
	return out, nil
}

// ehpcScanRegions are the regions ListHPCClusters asks: where E-HPC says it is
// offered, within the regions the account selected (all of them when none
// was). When E-HPC cannot say (the key may not call ListRegions, or the call
// failed) every region in scope is asked on its own endpoint instead, and one
// without a service answers with its absence (noEHPCEndpoint), not with
// another region's clusters.
func (d *Driver) ehpcScanRegions(ctx context.Context, creds provider.Credentials) ([]string, error) {
	offered, err := d.listEHPCRegions(ctx, creds)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return d.regions(ctx, creds)
	}
	chosen := selectedRegions(creds)
	if len(chosen) == 0 {
		sort.Strings(offered)
		return slices.Compact(offered), nil
	}
	var out []string
	for _, region := range chosen {
		if slices.Contains(offered, region) {
			out = append(out, region)
		}
	}
	return out, nil
}

var _ provider.HPCClusterProvider = (*Driver)(nil)

// ListHPCClusters implements provider.HPCClusterProvider: every E-HPC cluster
// of the account in the regions in scope, whether or not a node of it is
// listed as an instance (a cluster with no GPU node yet, or before a sync,
// is still one to connect). E-HPC says which regions it is offered in
// (ListRegions) and answers per region, so each of those is asked on its own
// endpoint, never on the central one, and one that fails does not stop the
// others. A cluster that more than one region returns (a private gateway that
// ignores the region) is listed once, in the first region in order:
//
//   - a region without an E-HPC endpoint, or where the account has not
//     activated E-HPC, has no clusters and is skipped;
//   - a region the key may not list (RAM) or that failed otherwise is a
//     Failed region of the result when other regions answered, so the list
//     is partial and says which;
//   - when no region answered, an account that activated E-HPC nowhere is
//     Access not_activated, one whose key may not list is Access
//     permission_missing, and any other failure is the error.
func (d *Driver) ListHPCClusters(ctx context.Context, creds provider.Credentials) (provider.HPCClusterList, error) {
	regions, err := d.ehpcScanRegions(ctx, creds)
	if err != nil {
		return provider.HPCClusterList{}, err
	}
	type outcome struct {
		clusters []provider.HPCClusterListing
		answered bool
		inactive string // the vendor's code when E-HPC is not activated here
		failure  *provider.HPCRegionFailure
		err      error
	}
	outcomes := make([]outcome, len(regions))
	var (
		wg    sync.WaitGroup
		slots = make(chan struct{}, regionConcurrency)
	)
	for i, region := range regions {
		wg.Add(1)
		go func(i int, region string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				outcomes[i].err = ctx.Err()
				return
			}
			defer func() { <-slots }()
			list, err := d.listEHPCClusters(ctx, creds, region, true)
			if err != nil {
				switch {
				case ctx.Err() != nil:
					outcomes[i].err = ctx.Err()
				case noEHPCEndpoint(err):
				case ehpcNotActivated(err) != "":
					outcomes[i].inactive = ehpcNotActivated(err)
				default:
					f := &provider.HPCRegionFailure{Region: region, Message: err.Error()}
					var apiErr *apiError
					if errors.As(err, &apiErr) {
						f.Code, f.Message = apiErr.Code, apiErr.Message
					}
					if perm := permissionError("ehpc", err); perm != nil {
						var denied *provider.SlurmApplyPermissionError
						errors.As(perm, &denied)
						f.Permission = denied.Permission
					}
					outcomes[i].failure, outcomes[i].err = f, fmt.Errorf("region %s: list E-HPC clusters: %w", region, err)
				}
				return
			}
			outcomes[i].answered = true
			for _, c := range list {
				info := hpcCluster(c.ClusterID, map[string]ehpcCluster{c.ClusterID: c})
				outcomes[i].clusters = append(outcomes[i].clusters, provider.HPCClusterListing{HPCCluster: *info, Region: region,
					State: c.ClusterStatus, Running: strings.EqualFold(strings.TrimSpace(c.ClusterStatus), "running"), ComputeNodes: c.Nodes.total()})
			}
		}(i, region)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return provider.HPCClusterList{}, err
	}
	var (
		out      provider.HPCClusterList
		answered bool
		inactive string
		denied   string
		firstErr error
		failures int
	)
	seen := map[string]bool{}
	for _, o := range outcomes {
		for _, c := range o.clusters {
			if !seen[c.ID] {
				seen[c.ID] = true
				out.Clusters = append(out.Clusters, c)
			}
		}
		answered = answered || o.answered
		if inactive == "" {
			inactive = o.inactive
		}
		if o.failure != nil {
			out.Failed = append(out.Failed, *o.failure)
			failures++
			if o.failure.Permission != "" {
				denied = o.failure.Permission
			} else if firstErr == nil {
				firstErr = o.err
			}
		}
	}
	switch {
	case answered || failures == 0 && inactive == "":
		return out, nil
	case failures == 0:
		return provider.HPCClusterList{Access: provider.HPCAccessNotActivated, Code: inactive}, nil
	case firstErr == nil:
		return provider.HPCClusterList{Access: provider.HPCAccessPermissionMissing, Permission: denied}, nil
	}
	return provider.HPCClusterList{}, firstErr
}

// ehpcNotActivated returns the vendor's code when err says the account has not
// activated E-HPC (the product is not opened for it), "" otherwise. The
// vendor's codes for this vary by product and version, so the family is
// matched: a code that says "not activated" or "not opened" in some spelling.
func ehpcNotActivated(err error) string {
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return ""
	}
	lower := strings.ToLower(apiErr.Code)
	for _, w := range []string{"notactivated", "notopen", "notenabled", "unactivated", "notsubscribed"} {
		if strings.Contains(lower, w) {
			return apiErr.Code
		}
	}
	return ""
}

// noEHPCEndpoint: the region has no E-HPC service, so its host does not
// resolve or the API says the region is not valid for the product.
func noEHPCEndpoint(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsNotFound {
		return true
	}
	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.Code
	return strings.HasPrefix(code, "InvalidRegionId")
}

// hpcCluster is the cluster a compute node is under: its ID from the node's
// tag, its name and scheduler from E-HPC when that was read.
func hpcCluster(id string, clusters map[string]ehpcCluster) *provider.HPCCluster {
	c, ok := clusters[id]
	if !ok {
		return &provider.HPCCluster{ID: id}
	}
	return &provider.HPCCluster{
		ID: id, Name: c.ClusterName,
		Scheduler:        strings.ToLower(strings.TrimSpace(c.Manager.Scheduler.Type)),
		SchedulerVersion: strings.TrimSpace(c.Manager.Scheduler.Version),
	}
}

// holdsEHPCNodes reports whether any of the region's instances is a
// compute node of an E-HPC cluster.
func holdsEHPCNodes(insts []apiInstance) bool {
	for _, inst := range insts {
		if inst.tag(ehpcClusterTag) != "" {
			return true
		}
	}
	return false
}
