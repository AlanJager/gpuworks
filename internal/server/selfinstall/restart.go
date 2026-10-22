package selfinstall

import (
	"context"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Existing is what the install questions offer as defaults when an
// installation is already in the directory.
type Existing struct {
	Config serverconfig.File
	Mode   string
}

// Existing reports the installation already in the layout's directory.
func (i *Installer) Existing(ctx context.Context) (Existing, bool) {
	inst, ok := i.previousInstall(ctx)
	if !ok {
		return Existing{}, false
	}
	return Existing{Config: inst.Config, Mode: inst.Mode}, true
}

// Restart stops and starts the installed server the way it is supervised and
// waits until it answers, so a configuration saved from the console (or edited
// in server.yaml) takes effect. It returns the service mode.
func (i *Installer) Restart(ctx context.Context) (string, error) {
	inst, err := i.readInstallation(ctx)
	if err != nil {
		return "", err
	}
	i.adoptLayout(inst.Config, false, false)
	svc := i.service(inst.Mode, inst.ServiceUser, i.unitConfig(inst.ServiceUser, inst.Mode))
	if inst.FromInstallJSON {
		if err := i.adoptInstallJSON(ctx, inst, svc); err != nil {
			return inst.Mode, err
		}
	}
	// Stopping first, rather than one `systemctl restart`, is what lets a
	// still-held address be told apart: another server answering /healthz
	// there would otherwise pass for this one having come back.
	if err := svc.Stop(ctx); err != nil {
		return inst.Mode, fmt.Errorf("stop the service: %w", err)
	}
	if err := i.checkAddrsReleased(ctx, inst.Config, "restart"); err != nil {
		return inst.Mode, err
	}
	if err := svc.Start(ctx); err != nil {
		return inst.Mode, err
	}
	probe, err := healthURL(installedProbeAddr(inst.Config))
	if err != nil {
		return inst.Mode, err
	}
	if err := i.waitHealthy(ctx, svc, probe, inst.ReleaseID); err != nil {
		i.io.Warn("the service did not become healthy; check %s", i.layout.LogFile())
		return inst.Mode, err
	}
	return inst.Mode, nil
}

// Stop shuts the installed server down. Under systemd or launchd it stays
// installed and comes back at the next boot; `gpuworks restart` starts it
// again at once.
func (i *Installer) Stop(ctx context.Context) (string, error) {
	inst, err := i.readInstallation(ctx)
	if err != nil {
		return "", err
	}
	i.adoptLayout(inst.Config, false, false)
	svc := i.service(inst.Mode, inst.ServiceUser, i.unitConfig(inst.ServiceUser, inst.Mode))
	if err := svc.Stop(ctx); err != nil {
		return inst.Mode, fmt.Errorf("stop the service: %w", err)
	}
	return inst.Mode, nil
}
