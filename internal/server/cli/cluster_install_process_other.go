//go:build !linux

package cli

import (
	"context"
	"errors"
)

func clusterInstallOwnedProcess(clusterInstallNodeRequest, string) (int, error) {
	return 0, errors.New("cluster installation node requires Linux")
}
func clusterInstallStart(clusterInstallNodeRequest, bool, []string) error {
	return errors.New("cluster installation node requires Linux")
}
func clusterInstallStop(context.Context, clusterInstallNodeRequest, string) error {
	return errors.New("cluster installation node requires Linux")
}

func clusterInstallProcessSupport() error {
	return errors.New("cluster installation node requires Linux pidfd support")
}

func clusterInstallStartContext(context.Context, clusterInstallNodeRequest, bool, []string) error {
	return errors.New("cluster installation node requires Linux")
}

func clusterInstallSupervisorCommandRun() error {
	return errors.New("cluster installation process supervision requires Linux")
}
