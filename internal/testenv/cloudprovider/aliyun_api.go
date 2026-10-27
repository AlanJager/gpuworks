package cloudprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	serverprovider "gpuworks.ai/gpuworks/internal/server/provider"
	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

// aliyunCaller is the signed OpenAPI transport. The product driver's RPC
// implements it; tests substitute the same wire shapes behind an HTTP fixture
// by pointing Credentials.Endpoint at it.
type aliyunCaller interface {
	Call(ctx context.Context, creds serverprovider.Credentials, product, region, action string, params map[string]string, out any) error
}

var _ aliyunCaller = (*aliyundriver.RPC)(nil)

// aliyunAPI is the slice of the ECS and VPC OpenAPIs the L4 provider uses.
// The product driver never creates or releases instances (it leaves that to
// the console), so this create/release surface lives with the tests.
type aliyunAPI struct {
	caller aliyunCaller
	creds  serverprovider.Credentials
}

func (a *aliyunAPI) ecs(ctx context.Context, region, action string, params map[string]string, out any) error {
	return a.caller.Call(ctx, a.creds, "ecs", region, action, params, out)
}

func (a *aliyunAPI) vpc(ctx context.Context, region, action string, params map[string]string, out any) error {
	return a.caller.Call(ctx, a.creds, "vpc", region, action, params, out)
}

// aliyunInstance is the part of DescribeInstances' Instance the provider reads.
type aliyunInstance struct {
	InstanceID      string `json:"InstanceId"`
	InstanceName    string `json:"InstanceName"`
	InstanceType    string `json:"InstanceType"`
	RegionID        string `json:"RegionId"`
	ZoneID          string `json:"ZoneId"`
	Status          string `json:"Status"`
	SpotStrategy    string `json:"SpotStrategy"`
	GPUAmount       int    `json:"GPUAmount"`
	GPUSpec         string `json:"GPUSpec"`
	PublicIPAddress struct {
		IPAddress []string `json:"IpAddress"`
	} `json:"PublicIpAddress"`
	VPCAttributes struct {
		PrivateIPAddress struct {
			IPAddress []string `json:"IpAddress"`
		} `json:"PrivateIpAddress"`
	} `json:"VpcAttributes"`
	OperationLocks struct {
		LockReason []struct {
			LockReason string `json:"LockReason"`
		} `json:"LockReason"`
	} `json:"OperationLocks"`
}

func (i aliyunInstance) publicIP() string {
	if len(i.PublicIPAddress.IPAddress) > 0 {
		return i.PublicIPAddress.IPAddress[0]
	}
	return ""
}

func (i aliyunInstance) privateIP() string {
	if len(i.VPCAttributes.PrivateIPAddress.IPAddress) > 0 {
		return i.VPCAttributes.PrivateIPAddress.IPAddress[0]
	}
	return ""
}

// reclaiming reports Alibaba Cloud's lock on a spot instance it is taking
// back: an interruption notice shows as the "Recycling" lock reason.
func (i aliyunInstance) reclaiming() bool {
	for _, lock := range i.OperationLocks.LockReason {
		if strings.EqualFold(lock.LockReason, "Recycling") {
			return true
		}
	}
	return false
}

func (a *aliyunAPI) regions(ctx context.Context) ([]string, error) {
	var out struct {
		Regions struct {
			Region []struct {
				RegionID string `json:"RegionId"`
			} `json:"Region"`
		} `json:"Regions"`
	}
	if err := a.ecs(ctx, "", "DescribeRegions", nil, &out); err != nil {
		return nil, err
	}
	var regions []string
	for _, region := range out.Regions.Region {
		regions = append(regions, region.RegionID)
	}
	sort.Strings(regions)
	return regions, nil
}

// describeInstances pages through one region. ids narrows to those instances;
// name filters by InstanceName and accepts the API's trailing "*" wildcard.
func (a *aliyunAPI) describeInstances(ctx context.Context, region string, ids []string, name string) ([]aliyunInstance, error) {
	var all []aliyunInstance
	token := ""
	for page := 0; page < 100; page++ {
		params := map[string]string{"MaxResults": "100"}
		if len(ids) > 0 {
			encoded, _ := json.Marshal(ids)
			params["InstanceIds"] = string(encoded)
		}
		if name != "" {
			params["InstanceName"] = name
		}
		if token != "" {
			params["NextToken"] = token
		}
		var out struct {
			Instances struct {
				Instance []aliyunInstance `json:"Instance"`
			} `json:"Instances"`
			NextToken string `json:"NextToken"`
		}
		if err := a.ecs(ctx, region, "DescribeInstances", params, &out); err != nil {
			return nil, err
		}
		for _, instance := range out.Instances.Instance {
			if instance.RegionID == "" {
				instance.RegionID = region
			}
			all = append(all, instance)
		}
		if out.NextToken == "" {
			return all, nil
		}
		token = out.NextToken
	}
	return nil, fmt.Errorf("aliyun: DescribeInstances in %s did not finish paging", region)
}

// stockKey identifies an instance type in one zone.
type stockKey struct{ zone, instanceType string }

// Stock categories of DescribeAvailableResource, best first.
const (
	stockWithStock       = "WithStock"
	stockClosedWithStock = "ClosedWithStock"
)

// stock returns the sellable instance types per zone of a region for one
// billing mode, keyed by zone and type, valued by StatusCategory. Only
// "Available" rows with stock are returned.
func (a *aliyunAPI) stock(ctx context.Context, region string, spot bool) (map[stockKey]string, error) {
	params := map[string]string{
		"DestinationResource": "InstanceType", "InstanceChargeType": "PostPaid",
		"ResourceType": "instance", "IoOptimized": "optimized", "NetworkCategory": "vpc",
		"SystemDiskCategory": aliyunSystemDiskCategory,
	}
	if spot {
		params["SpotStrategy"] = aliyunSpotStrategy
	}
	var out struct {
		AvailableZones struct {
			AvailableZone []struct {
				ZoneID             string `json:"ZoneId"`
				AvailableResources struct {
					AvailableResource []struct {
						SupportedResources struct {
							SupportedResource []struct {
								Value          string `json:"Value"`
								Status         string `json:"Status"`
								StatusCategory string `json:"StatusCategory"`
							} `json:"SupportedResource"`
						} `json:"SupportedResources"`
					} `json:"AvailableResource"`
				} `json:"AvailableResources"`
			} `json:"AvailableZone"`
		} `json:"AvailableZones"`
	}
	if err := a.ecs(ctx, region, "DescribeAvailableResource", params, &out); err != nil {
		return nil, err
	}
	stock := map[stockKey]string{}
	for _, zone := range out.AvailableZones.AvailableZone {
		for _, resource := range zone.AvailableResources.AvailableResource {
			for _, supported := range resource.SupportedResources.SupportedResource {
				if supported.Status != "Available" {
					continue
				}
				if supported.StatusCategory != stockWithStock && supported.StatusCategory != stockClosedWithStock {
					continue
				}
				stock[stockKey{zone.ZoneID, supported.Value}] = supported.StatusCategory
			}
		}
	}
	return stock, nil
}

const (
	aliyunSystemDiskCategory = "cloud_essd"
	aliyunSpotStrategy       = "SpotAsPriceGo"
	// aliyunInfraName names the long-lived shared network of the L4 tests:
	// VPC, vSwitches and security group. Nothing in it is run-owned, so
	// run-scoped cleanup never touches it.
	aliyunInfraName = "gpuworks-test-infra"
	// Tag keys on run-owned resources. The run tag is what attributes a stray
	// disk to a test run when its instance is already gone.
	aliyunRunTagKey   = "gpuworks-test-run"
	aliyunInfraTagKey = "gpuworks-test-infra"
)

type aliyunRunRequest struct {
	Region, Zone, InstanceType, ImageID string
	VSwitchID, SecurityGroupID          string
	Name, Password, RunID               string
	DiskGB                              int
	Spot                                bool
	// ClientToken makes a retried create idempotent: a transport error after
	// the request was accepted must not rent a second machine.
	ClientToken string
}

func (a *aliyunAPI) runInstance(ctx context.Context, req aliyunRunRequest) (string, error) {
	params := map[string]string{
		"ImageId": req.ImageID, "InstanceType": req.InstanceType, "ZoneId": req.Zone,
		"VSwitchId": req.VSwitchID, "SecurityGroupId": req.SecurityGroupID,
		"InstanceName": req.Name, "HostName": hostNameFor(req.Name),
		"Password":           req.Password,
		"InstanceChargeType": "PostPaid", "Amount": "1",
		"SystemDisk.Category": aliyunSystemDiskCategory, "SystemDisk.Size": strconv.Itoa(req.DiskGB),
		"InternetChargeType": "PayByTraffic", "InternetMaxBandwidthOut": "100",
		"IoOptimized": "optimized", "ClientToken": req.ClientToken,
		"Tag.1.Key": aliyunRunTagKey, "Tag.1.Value": req.RunID,
	}
	if req.Spot {
		params["SpotStrategy"] = aliyunSpotStrategy
		params["SpotInterruptionBehavior"] = "Terminate"
	}
	var out struct {
		InstanceIDSets struct {
			InstanceIDSet []string `json:"InstanceIdSet"`
		} `json:"InstanceIdSets"`
	}
	if err := a.ecs(ctx, req.Region, "RunInstances", params, &out); err != nil {
		return "", err
	}
	if len(out.InstanceIDSets.InstanceIDSet) != 1 {
		return "", fmt.Errorf("aliyun: RunInstances in %s returned %d instance IDs, want 1", req.Region, len(out.InstanceIDSets.InstanceIDSet))
	}
	return out.InstanceIDSets.InstanceIDSet[0], nil
}

// hostNameFor derives a valid Linux host name (letters, digits, hyphens; at
// most 30 characters) from the instance name.
func hostNameFor(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		}
	}
	host := strings.Trim(b.String(), "-")
	if len(host) > 30 {
		host = strings.Trim(host[:30], "-")
	}
	if host == "" {
		host = "gpuworks-test"
	}
	return host
}

func (a *aliyunAPI) deleteInstance(ctx context.Context, region, id string) error {
	return a.ecs(ctx, region, "DeleteInstance", map[string]string{"InstanceId": id, "Force": "true"}, nil)
}

// disk is a cloud disk found by run tag.
type aliyunDisk struct {
	DiskID string `json:"DiskId"`
	Status string `json:"Status"`
}

func (a *aliyunAPI) describeDisksByRun(ctx context.Context, region, runID string) ([]aliyunDisk, error) {
	var out struct {
		Disks struct {
			Disk []aliyunDisk `json:"Disk"`
		} `json:"Disks"`
	}
	params := map[string]string{"PageSize": "100", "Tag.1.Key": aliyunRunTagKey, "Tag.1.Value": runID}
	if err := a.ecs(ctx, region, "DescribeDisks", params, &out); err != nil {
		return nil, err
	}
	return out.Disks.Disk, nil
}

func (a *aliyunAPI) deleteDisk(ctx context.Context, region, diskID string) error {
	return a.ecs(ctx, region, "DeleteDisk", map[string]string{"DiskId": diskID}, nil)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
