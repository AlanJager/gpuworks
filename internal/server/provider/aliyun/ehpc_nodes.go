package aliyun

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Running a script on the GPU compute nodes of an E-HPC cluster. E-HPC lists
// the nodes (ListNodes: the ECS instance, the hostname, the cards), and Cloud
// Assistant, which runs on each of them as it does on the manager node, runs
// the script as root.

// slurmNodeCommandName names the invocation in the account's Cloud Assistant
// history.
const slurmNodeCommandName = "gpuworks-slurm-reach"

// slurmNodeParallel bounds the nodes asked at once.
const slurmNodeParallel = 8

// ehpcNodesPageSize is the ListNodes page size.
const ehpcNodesPageSize = 50

var _ provider.SlurmNodeProber = (*Driver)(nil)

// ehpcNode is a compute node as ListNodes (2024-07-30) lists it.
type ehpcNode struct {
	ID             string `json:"Id"`
	Hostname       string `json:"Hostname"`
	Status         string `json:"Status"`
	TotalResources struct {
		GPU int `json:"Gpu"`
	} `json:"TotalResources"`
}

// listEHPCNodes pages through the cluster's compute nodes (ListNodes).
func (d *Driver) listEHPCNodes(ctx context.Context, creds provider.Credentials, region, clusterID string) ([]ehpcNode, error) {
	var out []ehpcNode
	for page := 1; page <= 100; page++ {
		var resp struct {
			TotalCount int        `json:"TotalCount"`
			Nodes      []ehpcNode `json:"Nodes"`
		}
		params := map[string]string{"ClusterId": clusterID, "PageNumber": strconv.Itoa(page), "PageSize": strconv.Itoa(ehpcNodesPageSize)}
		if err := d.api.call(ctx, creds, ehpcProduct, region, "ListNodes", params, &resp); err != nil {
			if perm := permissionError("ehpc", err); perm != nil {
				return nil, perm
			}
			return nil, err
		}
		out = append(out, resp.Nodes...)
		if len(resp.Nodes) < ehpcNodesPageSize || len(out) >= resp.TotalCount {
			return out, nil
		}
	}
	return nil, errors.New("aliyun: ListNodes lists more nodes than gpuworks pages through")
}

var _ provider.SlurmNodeLister = (*Driver)(nil)

// SlurmNodeNames implements provider.SlurmNodeLister: the hostnames of every
// compute node ListNodes lists, whatever its state. A node listed without a
// hostname leaves the list not known whole, so it is an error.
func (d *Driver) SlurmNodeNames(ctx context.Context, creds provider.Credentials, region, clusterID string) ([]string, error) {
	nodes, err := d.listEHPCNodes(ctx, creds, region, clusterID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		name := strings.TrimSpace(n.Hostname)
		if name == "" {
			return nil, errors.New("aliyun: ListNodes lists a compute node without a hostname")
		}
		names = append(names, name)
	}
	return names, nil
}

// RunOnSlurmNodes implements provider.SlurmNodeProber: it runs script on every
// compute node of the cluster that has a card, up to slurmNodeParallel at a
// time. A node that is not running, whose Cloud Assistant is offline or whose
// command did not finish is answered as not asked, with why; the others
// carry what the script printed.
func (d *Driver) RunOnSlurmNodes(ctx context.Context, creds provider.Credentials, region, clusterID, script string, timeout time.Duration) ([]provider.SlurmNodeRun, error) {
	nodes, err := d.listEHPCNodes(ctx, creds, region, clusterID)
	if err != nil {
		return nil, err
	}
	var runs []provider.SlurmNodeRun
	var running []string
	for _, n := range nodes {
		if n.TotalResources.GPU <= 0 || strings.TrimSpace(n.ID) == "" {
			continue
		}
		run := provider.SlurmNodeRun{Name: n.Hostname, InstanceID: n.ID}
		if !strings.EqualFold(n.Status, "running") {
			run.NotAsked = provider.SlurmReachNodeNotRunning
		} else {
			running = append(running, n.ID)
		}
		runs = append(runs, run)
	}
	if len(running) == 0 {
		return runs, nil
	}
	online, err := d.assistantOnline(ctx, creds, region, running)
	if err != nil {
		if perm := permissionError("ecs", err); perm != nil {
			return nil, perm
		}
		return nil, err
	}
	var (
		wg    sync.WaitGroup
		slots = make(chan struct{}, slurmNodeParallel)
	)
	for i := range runs {
		run := &runs[i]
		if run.NotAsked != "" {
			continue
		}
		if !online[run.InstanceID] {
			run.NotAsked = provider.SlurmReachAssistantOffline
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				run.NotAsked = provider.SlurmReachCommandFailed
				return
			}
			defer func() { <-slots }()
			result, err := d.runCommand(ctx, creds, provider.Instance{ExternalID: run.InstanceID, Region: region}, slurmNodeCommandName, script, timeout)
			if err != nil || !result.succeeded() {
				run.NotAsked = provider.SlurmReachCommandFailed
				return
			}
			run.Output = result.output
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, err
	}
	return runs, nil
}
