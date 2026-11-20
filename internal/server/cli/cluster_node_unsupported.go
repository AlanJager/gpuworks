//go:build !linux || !dqlite

package cli

import (
	"context"
	"errors"
	"io"
)

var errClusterNodeUnsupported = errors.New("this command requires a Linux native dqlite executable")

func clusterInstallNodeAction(context.Context, clusterInstallNodeRequest, io.Writer) error {
	return errors.New("cluster-install requires a Linux native dqlite executable")
}

func clusterInitCommand(context.Context, []string, io.Writer) error { return errClusterNodeUnsupported }

func clusterJoinCommand(context.Context, []string, io.Writer) error { return errClusterNodeUnsupported }

func clusterAddressCommand(context.Context, []string, io.Writer) error {
	return errClusterNodeUnsupported
}

func statusCommand(context.Context, []string, io.Writer) error { return errClusterNodeUnsupported }
