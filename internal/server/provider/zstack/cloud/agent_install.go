package cloud

import (
	"context"
	"errors"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

func (d *Driver) InstallAgent(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) error {
	_, err := d.InstallAgentWithReport(ctx, creds, req)
	return err
}

func (d *Driver) InstallAgentWithReport(ctx context.Context, creds provider.Credentials, req provider.InstallRequest) (provider.InstallReport, error) {
	if d.store == nil || d.installer == nil {
		return provider.InstallReport{}, fmt.Errorf("Cloud installer is not configured")
	}
	auth := req.SSHAuth()
	if err := auth.Validate(); err != nil {
		return provider.InstallReport{}, err
	}
	if err := d.prepareHostForAgent(ctx, creds, req.Instance.ExternalID, req.Token); err != nil {
		return provider.InstallReport{}, err
	}
	report, err := d.installer.InstallSSH(ctx, provision.SSHInstallRequest{
		Target: req.Instance.PublicIP, Name: req.Instance.Name, User: req.Instance.SSHUser, Port: req.Instance.SSHPort,
		ProviderKind: d.Name(), InstanceKind: req.InstanceKind,
		SSHAuth: auth, ServerURL: req.ServerURL, Token: req.Token,
		ConnectMode: req.ConnectMode, ServerDialListen: req.ServerDialListen,
		ServerDialAddress: provision.ServerDialAddress(req.ServerDialAddress, req.ServerDialListen),
		InstallDir:        req.InstallDir, NoSystemd: req.NoSystemd,
	})
	if err != nil {
		var connectionErr *provision.SSHConnectionError
		if errors.As(err, &connectionErr) {
			return provider.InstallReport{}, provider.RetryableSSHConnectionError("Cloud SSH connection failed", connectionErr)
		}
		// Remote command output can contain credentials. Preserve cancellation,
		// but do not expose arbitrary SSH output in the management API.
		if ctx.Err() != nil {
			return provider.InstallReport{}, ctx.Err()
		}
		if errors.Is(err, provision.ErrInstallDirNotWritable) {
			return provider.InstallReport{}, fmt.Errorf("Cloud agent installation failed: %w %s", provision.ErrInstallDirNotWritable, req.InstallDir)
		}
		return provider.InstallReport{}, fmt.Errorf("Cloud agent installation failed")
	}
	return provider.ReportFromSSHInstall(report), nil
}

var _ provider.ReportingDriver = (*Driver)(nil)
