package cloudprovider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	aliyundriver "gpuworks.ai/gpuworks/internal/server/provider/aliyun"
)

// The L4 tests share one small network per region instead of creating a VPC
// per run: VPCs, vSwitches and security groups are free, count against low
// per-account quotas, and take a minute to create. Everything is found by its
// fixed name and created only when missing; run-scoped cleanup never touches
// it. The account's own VPCs are never read for reuse or modified.

const (
	aliyunInfraCIDR = "172.30.0.0/16"
	// aliyunAgentPort is where a server-dialed agent listens by default
	// (provision.DefaultServerDialListen), so the in-process test server can
	// dial the VM's public address.
	aliyunAgentPort = 18800
	infraPollEvery  = 2 * time.Second
	infraReadyWait  = 90 * time.Second
)

// aliyunNetwork is the shared network a VM in one zone is placed on.
type aliyunNetwork struct{ VPCID, VSwitchID, SecurityGroupID string }

type aliyunVPC struct {
	VPCID     string `json:"VpcId"`
	VPCName   string `json:"VpcName"`
	CIDRBlock string `json:"CidrBlock"`
	Status    string `json:"Status"`
}

type aliyunVSwitch struct {
	VSwitchID   string `json:"VSwitchId"`
	VSwitchName string `json:"VSwitchName"`
	ZoneID      string `json:"ZoneId"`
	CIDRBlock   string `json:"CidrBlock"`
	Status      string `json:"Status"`
}

func (a *aliyunAPI) describeVPCs(ctx context.Context, region string, params map[string]string) ([]aliyunVPC, error) {
	params["PageSize"] = "50"
	var out struct {
		Vpcs struct {
			Vpc []aliyunVPC `json:"Vpc"`
		} `json:"Vpcs"`
	}
	if err := a.vpc(ctx, region, "DescribeVpcs", params, &out); err != nil {
		return nil, err
	}
	return out.Vpcs.Vpc, nil
}

func (a *aliyunAPI) describeVSwitches(ctx context.Context, region, vpcID string) ([]aliyunVSwitch, error) {
	var out struct {
		VSwitches struct {
			VSwitch []aliyunVSwitch `json:"VSwitch"`
		} `json:"VSwitches"`
	}
	if err := a.vpc(ctx, region, "DescribeVSwitches", map[string]string{"VpcId": vpcID, "PageSize": "50"}, &out); err != nil {
		return nil, err
	}
	return out.VSwitches.VSwitch, nil
}

// ensureVPC returns the shared VPC of a region, creating it when missing.
func (a *aliyunAPI) ensureVPC(ctx context.Context, region string) (string, error) {
	vpcs, err := a.describeVPCs(ctx, region, map[string]string{"VpcName": aliyunInfraName})
	if err != nil {
		return "", fmt.Errorf("list VPCs in %s: %w", region, err)
	}
	for _, vpc := range vpcs {
		if vpc.VPCName == aliyunInfraName {
			return a.waitVPC(ctx, region, vpc.VPCID)
		}
	}
	var created struct {
		VPCID string `json:"VpcId"`
	}
	err = a.vpc(ctx, region, "CreateVpc", map[string]string{
		"CidrBlock": aliyunInfraCIDR, "VpcName": aliyunInfraName,
		"Description": "gpuworks L4 tests: long-lived shared network, safe to keep",
		"ClientToken": "gpuworks-test-vpc-" + region,
	}, &created)
	if err != nil {
		return "", fmt.Errorf("create shared VPC in %s: %w", region, err)
	}
	return a.waitVPC(ctx, region, created.VPCID)
}

func (a *aliyunAPI) waitVPC(ctx context.Context, region, vpcID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, infraReadyWait)
	defer cancel()
	for {
		vpcs, err := a.describeVPCs(ctx, region, map[string]string{"VpcId": vpcID})
		if err == nil && len(vpcs) == 1 && vpcs[0].Status == "Available" {
			return vpcID, nil
		}
		if sleepErr := sleepCtx(ctx, infraPollEvery); sleepErr != nil {
			return "", fmt.Errorf("wait for VPC %s in %s to become Available (last error %v): %w", vpcID, region, err, sleepErr)
		}
	}
}

// ensureVSwitch returns the shared vSwitch of a zone, creating it when missing.
func (a *aliyunAPI) ensureVSwitch(ctx context.Context, region, zone, vpcID string) (string, error) {
	name := aliyunInfraName + "-" + zone
	existing, err := a.describeVSwitches(ctx, region, vpcID)
	if err != nil {
		return "", fmt.Errorf("list vSwitches of %s: %w", vpcID, err)
	}
	for _, vswitch := range existing {
		if vswitch.VSwitchName == name && vswitch.ZoneID == zone {
			return a.waitVSwitch(ctx, region, vpcID, vswitch.VSwitchID)
		}
	}
	cidr, err := freeVSwitchCIDR(zone, existing)
	if err != nil {
		return "", err
	}
	var created struct {
		VSwitchID string `json:"VSwitchId"`
	}
	err = a.vpc(ctx, region, "CreateVSwitch", map[string]string{
		"VpcId": vpcID, "ZoneId": zone, "CidrBlock": cidr, "VSwitchName": name,
		"Description": "gpuworks L4 tests: long-lived shared network, safe to keep",
		"ClientToken": "gpuworks-test-vsw-" + zone,
	}, &created)
	if err != nil {
		return "", fmt.Errorf("create vSwitch %s (%s) in %s: %w", name, cidr, region, err)
	}
	return a.waitVSwitch(ctx, region, vpcID, created.VSwitchID)
}

func (a *aliyunAPI) waitVSwitch(ctx context.Context, region, vpcID, vswitchID string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, infraReadyWait)
	defer cancel()
	for {
		vswitches, err := a.describeVSwitches(ctx, region, vpcID)
		if err == nil {
			for _, vswitch := range vswitches {
				if vswitch.VSwitchID == vswitchID && vswitch.Status == "Available" {
					return vswitchID, nil
				}
			}
		}
		if sleepErr := sleepCtx(ctx, infraPollEvery); sleepErr != nil {
			return "", fmt.Errorf("wait for vSwitch %s in %s to become Available (last error %v): %w", vswitchID, region, err, sleepErr)
		}
	}
}

// freeVSwitchCIDR picks a /24 of the shared VPC no vSwitch uses, starting from
// a number derived from the zone letter so zones get stable, readable ranges.
func freeVSwitchCIDR(zone string, existing []aliyunVSwitch) (string, error) {
	used := map[string]bool{}
	for _, vswitch := range existing {
		used[vswitch.CIDRBlock] = true
	}
	start := 1
	if zone != "" {
		if last := zone[len(zone)-1]; last >= 'a' && last <= 'z' {
			start = int(last-'a') + 1
		}
	}
	for offset := 0; offset < 200; offset++ {
		cidr := "172.30." + strconv.Itoa(1+(start-1+offset)%200) + ".0/24"
		if !used[cidr] {
			return cidr, nil
		}
	}
	return "", fmt.Errorf("no free /24 left in the shared VPC %s", aliyunInfraCIDR)
}

// ensureSecurityGroup returns the shared security group of the VPC with the
// rules the tests need: SSH and the agent's server_dial port from ingressCIDR.
// Traffic between members needs no rule: instances of one "normal" security
// group reach each other on every port by default, which is what the two-VM
// cases rely on (NCCL, vLLM and the hosted control plane over the private
// network). The API refuses a rule naming a group as its own source
// (InvalidParamter.Conflict, 2026-10-25).
func (a *aliyunAPI) ensureSecurityGroup(ctx context.Context, region, vpcID, ingressCIDR string) (string, error) {
	var listed struct {
		SecurityGroups struct {
			SecurityGroup []struct {
				SecurityGroupID   string `json:"SecurityGroupId"`
				SecurityGroupName string `json:"SecurityGroupName"`
			} `json:"SecurityGroup"`
		} `json:"SecurityGroups"`
	}
	err := a.ecs(ctx, region, "DescribeSecurityGroups", map[string]string{"VpcId": vpcID, "SecurityGroupName": aliyunInfraName, "PageSize": "50"}, &listed)
	if err != nil {
		return "", fmt.Errorf("list security groups of %s: %w", vpcID, err)
	}
	groupID := ""
	for _, group := range listed.SecurityGroups.SecurityGroup {
		if group.SecurityGroupName == aliyunInfraName {
			groupID = group.SecurityGroupID
			break
		}
	}
	if groupID == "" {
		var created struct {
			SecurityGroupID string `json:"SecurityGroupId"`
		}
		err := a.ecs(ctx, region, "CreateSecurityGroup", map[string]string{
			"VpcId": vpcID, "SecurityGroupName": aliyunInfraName, "SecurityGroupType": "normal",
			"Description": "gpuworks L4 tests: long-lived shared group, safe to keep",
			"ClientToken": "gpuworks-test-sg-" + vpcID,
			"Tag.1.Key":   aliyunInfraTagKey, "Tag.1.Value": "1",
		}, &created)
		if err != nil {
			return "", fmt.Errorf("create shared security group in %s: %w", region, err)
		}
		groupID = created.SecurityGroupID
	}
	return groupID, a.ensureRules(ctx, region, groupID, ingressCIDR)
}

type aliyunPermission struct {
	IPProtocol    string `json:"IpProtocol"`
	PortRange     string `json:"PortRange"`
	SourceCIDRIP  string `json:"SourceCidrIp"`
	SourceGroupID string `json:"SourceGroupId"`
	Policy        string `json:"Policy"`
}

func (a *aliyunAPI) ensureRules(ctx context.Context, region, groupID, ingressCIDR string) error {
	var attr struct {
		Permissions struct {
			Permission []aliyunPermission `json:"Permission"`
		} `json:"Permissions"`
	}
	err := a.ecs(ctx, region, "DescribeSecurityGroupAttribute", map[string]string{"SecurityGroupId": groupID, "Direction": "ingress"}, &attr)
	if err != nil {
		return fmt.Errorf("read rules of %s: %w", groupID, err)
	}
	have := func(ports string) bool {
		for _, rule := range attr.Permissions.Permission {
			if strings.EqualFold(rule.IPProtocol, "tcp") && rule.PortRange == ports && rule.SourceCIDRIP == ingressCIDR && strings.EqualFold(rule.Policy, "accept") {
				return true
			}
		}
		return false
	}
	for _, port := range []int{22, aliyunAgentPort} {
		ports := fmt.Sprintf("%d/%d", port, port)
		if have(ports) {
			continue
		}
		params := map[string]string{
			"SecurityGroupId": groupID, "IpProtocol": "tcp", "PortRange": ports, "SourceCidrIp": ingressCIDR,
			"Policy": "accept", "Priority": "1", "Description": "gpuworks L4 tests",
		}
		if err := a.ecs(ctx, region, "AuthorizeSecurityGroup", params, nil); err != nil && !strings.Contains(aliyundriver.ErrorCode(err), "Duplicate") {
			return fmt.Errorf("authorize tcp %s on %s: %w", ports, groupID, err)
		}
	}
	return nil
}
