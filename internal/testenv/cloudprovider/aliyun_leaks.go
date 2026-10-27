package cloudprovider

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
)

// scan lists instances whose name matches (a trailing * is a wildcard)
// across the scan regions, recording where each was found.
func (p *Aliyun) scan(ctx context.Context, name string) ([]aliyunInstance, error) {
	regions, explicit, err := p.scanRegions(ctx)
	if err != nil {
		return nil, err
	}
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		slots    = make(chan struct{}, aliyunRegionConcurrency)
		found    []aliyunInstance
		failures []string
	)
	for _, region := range regions {
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			// A region that answers badly once may answer on the next try; only
			// a region that keeps failing is reported (or skipped, see below).
			var instances []aliyunInstance
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				if instances, err = p.api.describeInstances(ctx, region, nil, name); err == nil || p.sleep(ctx, p.poll) != nil {
					break
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", region, err))
				return
			}
			found = append(found, instances...)
		}(region)
	}
	wg.Wait()
	sort.Strings(failures)
	switch {
	case len(failures) > 0 && (explicit || len(failures) == len(regions)):
		return nil, fmt.Errorf("list aliyun instances: %s", strings.Join(failures, "; "))
	case len(failures) > 0:
		// A region the account cannot use must not block everyone's cleanup;
		// say so and carry on with the rest.
		fmt.Fprintf(os.Stderr, "aliyun: skipped %d region(s) that could not be listed: %s\n", len(failures), strings.Join(failures, "; "))
	}
	sort.Slice(found, func(i, j int) bool { return found[i].InstanceID < found[j].InstanceID })
	p.mu.Lock()
	for _, instance := range found {
		p.regionOf[instance.InstanceID] = instance.RegionID
	}
	p.mu.Unlock()
	return found, nil
}

// ListInstances exposes the run's and every other test run's instances
// (name prefix gpuworks-test-) for diagnostics.
func (p *Aliyun) ListInstances(ctx context.Context) ([]serverprovider.Instance, error) {
	found, err := p.scan(ctx, TestNamePrefix+"*")
	if err != nil {
		return nil, err
	}
	out := make([]serverprovider.Instance, 0, len(found))
	for _, instance := range found {
		power := serverprovider.PowerUnknown
		switch instance.Status {
		case "Running":
			power = serverprovider.PowerRunning
		case "Stopped":
			power = serverprovider.PowerStopped
		}
		out = append(out, serverprovider.Instance{
			ExternalID: instance.InstanceID, Name: instance.InstanceName, GPUModel: instance.GPUSpec, GPUCount: instance.GPUAmount,
			Power: power, Region: instance.RegionID, Zone: instance.ZoneID, PublicIP: instance.publicIP(),
			PrivateIP: instance.privateIP(), SSHUser: "root", SSHPort: 22, Kind: "vm",
		})
	}
	return out, nil
}

func (p *Aliyun) ListLeaked(ctx context.Context) ([]string, error) {
	found, err := p.scan(ctx, TestNamePrefix+"*")
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(found))
	for _, instance := range found {
		if strings.HasPrefix(instance.InstanceName, TestNamePrefix) {
			ids = append(ids, instance.InstanceID)
		}
	}
	return ids, nil
}

// ListLeakedForRunID lists only the VMs named for runID. The wildcard query
// is filtered again by exact name: the API's wildcard is a prefix match.
func (p *Aliyun) ListLeakedForRunID(ctx context.Context, runID string) ([]string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, fmt.Errorf("%s: cleanup run ID is required", p.Name())
	}
	found, err := p.scan(ctx, TestNamePrefix+runID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, instance := range found {
		if instance.InstanceName == TestNamePrefix+runID {
			ids = append(ids, instance.InstanceID)
		}
	}
	return ids, nil
}

// ListOrphanDisksForRunID returns disks tagged with the run that no longer
// belong to a released instance, as "region/disk-id". The system disk goes
// with its instance (DeleteWithInstance), so a result is a leak.
func (p *Aliyun) ListOrphanDisksForRunID(ctx context.Context, runID string) ([]string, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, fmt.Errorf("%s: cleanup run ID is required", p.Name())
	}
	regions, explicit, err := p.scanRegions(ctx)
	if err != nil {
		return nil, err
	}
	var disks []string
	failed := 0
	for _, region := range regions {
		listed, err := p.api.describeDisksByRun(ctx, region, runID)
		if err != nil {
			if explicit {
				return nil, fmt.Errorf("list disks of run %s in %s: %w", runID, region, err)
			}
			// Same leniency as the instance scan: a region the key cannot use
			// must not block cleanup, unless no region could be read at all.
			failed++
			if failed == len(regions) {
				return nil, fmt.Errorf("list disks of run %s: no region could be read (last: %s: %w)", runID, region, err)
			}
			continue
		}
		for _, disk := range listed {
			disks = append(disks, region+"/"+disk.DiskID)
		}
	}
	return disks, nil
}

// DeleteOrphanDisk deletes one "region/disk-id" from ListOrphanDisksForRunID.
func (p *Aliyun) DeleteOrphanDisk(ctx context.Context, regionAndID string) error {
	region, id, ok := strings.Cut(regionAndID, "/")
	if !ok || region == "" || id == "" {
		return fmt.Errorf("aliyun: invalid disk reference %q", regionAndID)
	}
	return p.api.deleteDisk(ctx, region, id)
}

// Interrupted reports whether a spot instance was (or is being) taken back by
// Alibaba Cloud: it vanished, stopped, or carries the Recycling lock.
func (p *Aliyun) Interrupted(ctx context.Context, instanceID string) (bool, string) {
	p.mu.Lock()
	region, ok := p.regionOf[instanceID]
	p.mu.Unlock()
	if !ok {
		return false, ""
	}
	found, err := p.api.describeInstances(ctx, region, []string{instanceID}, "")
	switch {
	case err != nil:
		return false, ""
	case len(found) == 0:
		return true, "instance " + instanceID + " no longer exists; Alibaba Cloud reclaimed the spot capacity (or it was released)"
	case found[0].reclaiming():
		return true, "instance " + instanceID + " is being reclaimed by Alibaba Cloud (Recycling lock)"
	case found[0].Status == "Stopped" && found[0].SpotStrategy != "" && found[0].SpotStrategy != "NoSpot":
		return true, "spot instance " + instanceID + " was stopped by Alibaba Cloud"
	}
	return false, ""
}
