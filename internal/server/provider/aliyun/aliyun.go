// Package aliyun implements the provider.Driver for Alibaba Cloud ECS. It
// signs requests with the OpenAPI V3 signature, using either an AccessKey or
// STS credentials for a RAM role, and installs the agent through Cloud
// Assistant (云助手), which needs neither SSH nor a public address, falling
// back to SSH on instances whose Cloud Assistant is offline. Operator docs:
// docs/how-to/rent-cloud-gpus.md#阿里云.
package aliyun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

func init() { provider.Register(New()) }

// Driver is the Alibaba Cloud ECS provider driver.
type Driver struct {
	api *client
	// assistantWait and assistantPoll override how long and how often an
	// install waits for Cloud Assistant to come online (tests).
	assistantWait, assistantPoll time.Duration
}

// New returns a Driver with production defaults.
func New() *Driver { return &Driver{api: newClient()} }

// Name implements provider.Driver.
func (d *Driver) Name() string { return "aliyun" }

// Capabilities implements provider.Driver. Creating and releasing instances
// is left to the Alibaba Cloud console: the console's choices (VPC, security
// group, image, billing) are the operator's, and releasing destroys data.
func (d *Driver) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		CanListInstances: true,
		CanPowerControl:  true,
		CanInstallAgent:  true,
	}
}

// ValidateCredentials lists the regions: it needs only ecs:DescribeRegions and
// proves the key (and the role, when one is set) work.
func (d *Driver) ValidateCredentials(ctx context.Context, creds provider.Credentials) error {
	_, err := d.regions(ctx, creds)
	return err
}

// PowerState implements provider.Driver.
func (d *Driver) PowerState(ctx context.Context, creds provider.Credentials, externalID string) (provider.PowerState, error) {
	inst, err := d.findInstance(ctx, creds, externalID)
	if err != nil {
		return provider.PowerUnknown, err
	}
	return mapPower(inst.Status), nil
}

// PowerOn starts a stopped instance.
func (d *Driver) PowerOn(ctx context.Context, creds provider.Credentials, externalID string) error {
	inst, err := d.findInstance(ctx, creds, externalID)
	if err != nil {
		return err
	}
	return d.api.call(ctx, creds, ecsProduct, inst.RegionID, "StartInstance", map[string]string{"InstanceId": inst.InstanceID}, nil)
}

// PowerOff stops an instance and keeps charging for it: the alternative
// (StopCharging, 节省停机) releases the instance's public IP and its GPUs,
// which may not be available again when it starts.
func (d *Driver) PowerOff(ctx context.Context, creds provider.Credentials, externalID string) error {
	inst, err := d.findInstance(ctx, creds, externalID)
	if err != nil {
		return err
	}
	return d.api.call(ctx, creds, ecsProduct, inst.RegionID, "StopInstance", map[string]string{"InstanceId": inst.InstanceID, "StoppedMode": "KeepCharging"}, nil)
}

// CreateInstance is not offered; see Capabilities.
func (d *Driver) CreateInstance(context.Context, provider.Credentials, provider.CreateInstanceRequest) (provider.Instance, error) {
	return provider.Instance{}, provider.ErrUnsupported
}

// DeleteInstance is not offered; see Capabilities.
func (d *Driver) DeleteInstance(context.Context, provider.Credentials, string) error {
	return provider.ErrUnsupported
}

// ResizeInstance is not offered.
func (d *Driver) ResizeInstance(context.Context, provider.Credentials, string, provider.ResizeInstanceRequest) error {
	return provider.ErrUnsupported
}

// InstallAgent implements provider.Driver through InstallAgentWithReport.
func (d *Driver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

// findInstance looks an instance up by ID across the regions in scope: the
// framework hands drivers the ID alone.
func (d *Driver) findInstance(ctx context.Context, creds provider.Credentials, externalID string) (apiInstance, error) {
	externalID = strings.TrimSpace(externalID)
	if externalID == "" {
		return apiInstance{}, errors.New("aliyun: instance id is required")
	}
	regions, err := d.regions(ctx, creds)
	if err != nil {
		return apiInstance{}, err
	}
	found, err := forEachRegion(ctx, regions, func(ctx context.Context, region string) ([]apiInstance, error) {
		return d.describeInstances(ctx, creds, region, map[string]string{"InstanceIds": fmt.Sprintf("[%q]", externalID)})
	})
	if err != nil {
		return apiInstance{}, err
	}
	for _, inst := range found {
		if inst.InstanceID == externalID {
			return inst, nil
		}
	}
	return apiInstance{}, fmt.Errorf("aliyun: instance %s not found", externalID)
}
