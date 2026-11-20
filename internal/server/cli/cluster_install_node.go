package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
)

// clusterDatabaseHelperName is the replicated database helper executable an
// installation keeps beside its server executable.
const clusterDatabaseHelperName = "gpuworks-dqlite"

// clusterInstallSupervisorCommand is the internal command that owns one
// temporary installation process and records its exit.
const clusterInstallSupervisorCommand = "cluster-install-supervise"

// clusterInstallRuntimeAvailable reports whether this executable can run the
// replicated control plane from its installation directory. It is the single
// decision behind the native_dqlite capability and the refusal of every later
// action; today the runtime is linked into the executable.
func clusterInstallRuntimeAvailable() bool {
	return clusterInstallNative
}

func clusterInstallNodeCommand(ctx context.Context, input io.Reader, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var req clusterInstallNodeRequest
	decoder := json.NewDecoder(io.LimitReader(input, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return errors.New("invalid cluster installation request")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected cluster installation input")
	}
	if req.Action == "capabilities" {
		return json.NewEncoder(output).Encode(map[string]any{
			"native_dqlite": clusterInstallRuntimeAvailable(),
			"os":            runtime.GOOS,
			"arch":          runtime.GOARCH,
		})
	}
	// Refuse before reading or writing anything in the installation directory.
	if !clusterInstallRuntimeAvailable() {
		return errors.New("cluster-install requires a Linux native dqlite executable")
	}
	return clusterInstallNodeAction(ctx, req, output)
}

// clusterInstallFreshEntries accepts exactly what the installer copies before
// prepare: the server executable and, when supplied, the database helper.
func clusterInstallFreshEntries(entries []os.DirEntry) bool {
	server := false
	for _, entry := range entries {
		switch entry.Name() {
		case "gpuworks":
			server = true
		case clusterDatabaseHelperName:
		default:
			return false
		}
		if !entry.Type().IsRegular() {
			return false
		}
	}
	return server
}
