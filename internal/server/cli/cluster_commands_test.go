package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// clusterCommandNames are the control-plane cluster commands. This build has
// none of them: a separately built server that ships the clustered runtime
// registers them through Options.Commands.
var clusterCommandNames = []string{
	"cluster", "cluster-install", "cluster-check", "cluster-install-node", "cluster-check-node",
	"cluster-install-supervise", "cluster-init", "cluster-address", "cluster-join", "cluster-status",
}

func TestCommunityBuildHasNoClusterCommand(t *testing.T) {
	var usage bytes.Buffer
	usageTo(&usage, nil)
	for _, line := range strings.Split(usage.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "gpuworks cluster") {
			t.Fatalf("the community usage lists a cluster command: %q", line)
		}
	}
	for _, name := range clusterCommandNames {
		var stderr bytes.Buffer
		if code := Run(discardLog(), []string{name}, &stderr, Options{}); code != 2 {
			t.Fatalf("%s: exit code = %d, want 2 (unknown subcommand)\n%s", name, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "unknown subcommand") {
			t.Fatalf("%s was not refused as unknown:\n%s", name, stderr.String())
		}
	}
}

func TestABuildRegistersTheClusterCommands(t *testing.T) {
	var ran []string
	var opt Options
	for _, name := range clusterCommandNames {
		opt.Commands = append(opt.Commands, Command{
			Name:  name,
			Usage: name + " [flags]",
			Run: func(_ context.Context, _ Env, args []string) error {
				ran = append(ran, name+" "+strings.Join(args, " "))
				return nil
			},
		})
	}
	for _, name := range clusterCommandNames {
		var stderr bytes.Buffer
		if code := Run(discardLog(), []string{name, "--data", "/srv"}, &stderr, opt); code != 0 {
			t.Fatalf("%s: exit code = %d\n%s", name, code, stderr.String())
		}
	}
	if len(ran) != len(clusterCommandNames) {
		t.Fatalf("ran %q, want every cluster command once", ran)
	}
	for i, name := range clusterCommandNames {
		if ran[i] != name+" --data /srv" {
			t.Fatalf("command %d ran as %q, want %q with its arguments", i, ran[i], name)
		}
	}
	var usage bytes.Buffer
	usageTo(&usage, opt.Commands)
	for _, name := range clusterCommandNames {
		if !strings.Contains(usage.String(), "\n  gpuworks "+name+" [flags]") {
			t.Fatalf("the usage does not list %s:\n%s", name, usage.String())
		}
	}
}
