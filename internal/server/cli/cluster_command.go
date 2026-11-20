package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
)

const clusterCommandUsage = `  gpuworks cluster install [--configure | --interactive] [--config cluster.yaml]
  gpuworks cluster install --config cluster.yaml --binary HA_BINARY [--password-file FILE]
  gpuworks cluster install --config cluster.yaml --binary HA_BINARY --database-helper HELPER [--password-file FILE]
                           [--storage-tier standard|slow-storage] [--storage-probe 30s]
  gpuworks cluster check --config cluster.yaml --binary HA_BINARY
  gpuworks cluster-install and gpuworks cluster-check are the same commands.
  On one member of a Linux HA build:
  gpuworks cluster-init --offline --listen IP:PORT --public-url https://HOST:PORT [--storage-tier standard|slow-storage] [--data ./data]
  gpuworks cluster-address --offline --address HOST:PORT [--data ./data]
  gpuworks cluster-join --address HOST:PORT --cluster-id ID --ca-fingerprint FP --enrollment-url https://HOST:PORT [--data ./data]
  gpuworks cluster-status [--data ./data] [--http-addr HOST:PORT] [--timeout 5s]
`

func runClusterCommand(log *slog.Logger, args []string, output io.Writer) int {
	if len(args) == 0 {
		clusterUsageTo(output)
		return 2
	}
	switch args[0] {
	case "install", "check":
		// Only these cluster operations share the existing flat dispatch paths.
		return Run(log, append([]string{"cluster-" + args[0]}, args[1:]...), output, Options{})
	case "help", "-h", "--help":
		clusterUsageTo(output)
		return 0
	default:
		fmt.Fprintf(output, "unknown cluster subcommand %q\n", args[0])
		clusterUsageTo(output)
		return 2
	}
}

func clusterUsageTo(output io.Writer) {
	fmt.Fprintf(output, "usage:\n%s\nsubcommands:\n  install  configure and install a three-node HA cluster over SSH\n  check    preflight a three-node HA cluster over SSH\n\nUse gpuworks cluster <subcommand> --help for flags.\ncluster-install and cluster-check remain compatible aliases.\n", clusterCommandUsage)
}

func clusterInstallExit(log *slog.Logger, args []string, stderr io.Writer) int {
	if err := clusterInstallCommand(cmdContext(), args, stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Error("cluster installation failed; remote data and logs retained", "err", err)
		return 1
	}
	return 0
}

func clusterCheckExit(log *slog.Logger, args []string, stderr io.Writer) int {
	if err := clusterCheckCommand(cmdContext(), args, stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Error("cluster preflight failed", "err", err)
		return 1
	}
	return 0
}

func clusterInstallNodeExit(log *slog.Logger, args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "cluster-install-node reads one JSON request on stdin and accepts no arguments")
		return 2
	}
	if err := clusterInstallNodeCommand(cmdContext(), os.Stdin, os.Stdout); err != nil {
		log.Error("cluster installation node operation failed", "err", err)
		return 1
	}
	return 0
}

func clusterCheckNodeExit(log *slog.Logger, args []string, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "cluster-check-node reads one JSON request on stdin and accepts no arguments")
		return 2
	}
	if err := clusterCheckNodeCommand(cmdContext(), os.Stdin, os.Stdout); err != nil {
		log.Error("cluster preflight node operation failed", "err", err)
		return 1
	}
	return 0
}

// clusterNodeExit runs one member-local cluster command. Its result goes to
// output (stdout in the process), because installers parse it.
func clusterNodeExit(log *slog.Logger, name string, args []string, output io.Writer) int {
	commands := map[string]func(context.Context, []string, io.Writer) error{
		"cluster-init":    clusterInitCommand,
		"cluster-address": clusterAddressCommand,
		"cluster-join":    clusterJoinCommand,
		"cluster-status":  statusCommand,
	}
	if err := commands[name](cmdContext(), args, output); err != nil && !errors.Is(err, flag.ErrHelp) {
		log.Error(name+" failed", "err", err)
		return 1
	}
	return 0
}
