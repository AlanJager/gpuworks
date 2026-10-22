package provision

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// listeningPortsCommand prints the kernel's TCP socket tables, which every
// Linux has, where ss and netstat may be missing from a container image.
const listeningPortsCommand = "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null"

// ListeningPorts are the TCP ports something on the host listens on: before
// a provider's mapped port is given to a server-dialed agent, it must be one
// nothing else holds.
func ListeningPorts(ctx context.Context, client *ssh.Client) (map[int]bool, error) {
	out, err := run(ctx, client, listeningPortsCommand)
	if err != nil {
		return nil, fmt.Errorf("list listening ports: %w", err)
	}
	ports := parseListeningPorts(out)
	if len(ports) == 0 && !strings.Contains(out, "local_address") {
		return nil, fmt.Errorf("list listening ports: the host has no /proc/net/tcp")
	}
	return ports, nil
}

// parseListeningPorts reads /proc/net/tcp{,6}: the local address's port is
// hex after the colon, and state 0A is LISTEN.
func parseListeningPorts(out string) map[int]bool {
	ports := map[int]bool{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != "0A" {
			continue
		}
		_, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		if port, err := strconv.ParseUint(hexPort, 16, 16); err == nil {
			ports[int(port)] = true
		}
	}
	return ports
}
