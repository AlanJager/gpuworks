package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProxyCommand descendants can retain output pipes after SSH is canceled.
// Bound pipe draining as well as the SSH process lifetime.
func clusterInstallSSHCommand(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func clusterInstallSSHArgs(node clusterInstallNode) []string {
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=8", "-o", "StrictHostKeyChecking=yes"}
	if node.SSH.Key != "" {
		args = append(args, "-i", node.SSH.Key)
	}
	if node.SSH.User != "" {
		args = append(args, "-l", node.SSH.User)
	}
	if node.SSH.Port != 0 {
		args = append(args, "-p", fmt.Sprint(node.SSH.Port))
	}
	return append(args, node.SSH.Target)
}

// Resolve the account used by OpenSSH so the remote helper and systemd unit
// agree with SSH config, including per-host User settings.
func resolveClusterInstallSSH(ctx context.Context, o *clusterInstallOptions) error {
	if len(o.Nodes) == 0 {
		for _, host := range o.Hosts {
			o.Nodes = append(o.Nodes, o.node(host))
		}
	}
	var failures []error
	for i := range o.Nodes {
		node := &o.Nodes[i]
		if node.SSH.Key != "" {
			info, err := os.Stat(node.SSH.Key)
			if err != nil || !info.Mode().IsRegular() {
				failures = append(failures, fmt.Errorf("%s: SSH key must be an existing regular file", node.Name))
				continue
			}
			key, err := os.Open(node.SSH.Key)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: cannot read configured SSH key: %w", node.Name, err))
				continue
			}
			if err := key.Close(); err != nil {
				failures = append(failures, fmt.Errorf("%s: close configured SSH key: %w", node.Name, err))
				continue
			}
		}
		args := append([]string{"-G"}, clusterInstallSSHArgs(*node)...)
		raw, err := clusterInstallSSHCommand(ctx, args).Output()
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: cannot resolve SSH configuration: %w", node.Name, err))
			continue
		}
		user := ""
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "user" {
				user = fields[1]
				break
			}
		}
		if !clusterInstallUserPattern.MatchString(user) {
			failures = append(failures, fmt.Errorf("%s: OpenSSH did not resolve a valid Unix account", node.Name))
			continue
		}
		node.SSH.User = user
		if node.Dir == "" {
			node.Dir = clusterInstallDefaultDir(user, o.NoService)
		}
	}
	return errors.Join(failures...)
}

func clusterInstallTransports(o clusterInstallOptions) (clusterInstallRunner, func(context.Context, string, string, io.Reader) error) {
	remote := func(ctx context.Context, host string, input io.Reader) ([]byte, error) {
		node := o.node(host)
		args := append(clusterInstallSSHArgs(node), clusterInstallQuote(node.Dir+"/gpuworks")+" cluster-install-node")
		cmd := clusterInstallSSHCommand(ctx, args)
		cmd.Stdin = input
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("remote operation on %s failed; retained files: %s: %w", host, node.Dir, err)
		}
		return raw, nil
	}
	shell := func(ctx context.Context, host, command string, input io.Reader) error {
		args := append(clusterInstallSSHArgs(o.node(host)), command)
		cmd := clusterInstallSSHCommand(ctx, args)
		cmd.Stdin = input
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("SSH preflight/copy on %s failed (verify SSH host key, permissions and ports): %w", host, err)
		}
		return nil
	}
	return remote, shell
}

// OpenSSH owns identity/agent selection. A local -G read can suggest a
// directory and show settings, but it does not prove remote authentication.
func clusterInstallSSHConfig(ctx context.Context, node clusterInstallNode) (string, int, error) {
	args := append([]string{"-G"}, clusterInstallSSHArgs(node)...)
	raw, err := clusterInstallSSHCommand(ctx, args).Output()
	if err != nil {
		return "", 0, fmt.Errorf("cannot read local OpenSSH settings: %w", err)
	}
	user, port := "", 0
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "user":
			user = fields[1]
		case "port":
			port, _ = strconv.Atoi(fields[1])
		}
	}
	if !clusterInstallUserPattern.MatchString(user) || port < 1 || port > 65535 {
		return "", 0, errors.New("OpenSSH did not resolve a valid Unix account and port")
	}
	return user, port, nil
}
