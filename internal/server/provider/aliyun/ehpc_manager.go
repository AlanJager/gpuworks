package aliyun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
)

// Running the Slurm setup on an E-HPC cluster's manager node. The manager is
// an ECS instance like the compute nodes, so Cloud Assistant runs a script
// there as root (provider.SlurmManagerScript); E-HPC itself says which
// instance it is (GetCluster).

// slurmManagerCommandName names the invocation in the account's Cloud
// Assistant history.
const slurmManagerCommandName = "gpuworks-slurm-setup"

var _ provider.SlurmManagerRunner = (*Driver)(nil)

// RunOnSlurmManager implements provider.SlurmManagerRunner.
func (d *Driver) RunOnSlurmManager(ctx context.Context, creds provider.Credentials, region, clusterID, script string, timeout time.Duration) (provider.SlurmManagerRun, error) {
	managerID, err := d.ehpcManagerInstance(ctx, creds, region, clusterID)
	if err != nil {
		return provider.SlurmManagerRun{}, err
	}
	manager := provider.Instance{ExternalID: managerID, Region: region}
	if err := d.requireAssistant(ctx, creds, manager); err != nil {
		return provider.SlurmManagerRun{}, applyAPIError(messages.SlurmApplyStepAssistant, err)
	}
	result, err := d.runCommand(ctx, creds, manager, slurmManagerCommandName, script, timeout)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("gave up waiting for the command's result: %w", ctx.Err())
		}
		return provider.SlurmManagerRun{}, applyAPIError(messages.SlurmApplyStepRun, err)
	}
	run := provider.SlurmManagerRun{ManagerInstance: managerID, Output: result.output}
	if !result.succeeded() {
		run.Ended = "the command " + result.describe()
	}
	return run, nil
}

// ehpcManagerInstance is the ECS instance of the cluster's manager node,
// from E-HPC's GetCluster (Manager.ManagerNode.InstanceId).
func (d *Driver) ehpcManagerInstance(ctx context.Context, creds provider.Credentials, region, clusterID string) (string, error) {
	var resp struct {
		Manager struct {
			ManagerNode struct {
				InstanceID string `json:"InstanceId"`
			} `json:"ManagerNode"`
		} `json:"Manager"`
	}
	if err := d.api.call(ctx, creds, ehpcProduct, region, "GetCluster", map[string]string{"ClusterId": clusterID}, &resp); err != nil {
		if perm := permissionError("ehpc", err); perm != nil {
			return "", perm
		}
		return "", &provider.SlurmApplyFailedError{Step: messages.SlurmApplyStepCluster, Output: tail(err.Error())}
	}
	id := strings.TrimSpace(resp.Manager.ManagerNode.InstanceID)
	if id == "" {
		return "", provider.ErrSlurmApplyManagerUnknown
	}
	return id, nil
}

// applyAPIError is err as the setup reports it: the permission a RAM policy
// lacks, or the failed step with what the cloud said.
func applyAPIError(step string, err error) error {
	if perm := permissionError("ecs", err); perm != nil {
		return perm
	}
	return &provider.SlurmApplyFailedError{Step: step, Output: tail(err.Error())}
}

// permissionError is err as a missing RAM permission ("<product>:<Action>")
// when the API refused the key for lack of it, nil for any other error.
func permissionError(product string, err error) error {
	var apiErr *apiError
	if !errors.As(err, &apiErr) || apiErr.Action == "" {
		return nil
	}
	if code := apiErr.Code; !strings.HasPrefix(code, "Forbidden") && !strings.HasPrefix(code, "NoPermission") && !strings.HasPrefix(code, "NotAuthorized") {
		return nil
	}
	return &provider.SlurmApplyPermissionError{Permission: product + ":" + apiErr.Action, Err: err}
}
