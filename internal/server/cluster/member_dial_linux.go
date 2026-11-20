//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"gopkg.in/yaml.v3"
)

// dialMember opens one connection to a member's database address. With TLS
// the handshake names the dialed host: tls.Client, unlike tls.Dial, does not
// derive the server name, and verification then fails for every member.
func dialMember(ctx context.Context, config *tls.Config, address string) (net.Conn, error) {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	// A blackholed member must fail a call instead of holding it until the
	// kernel's default retransmission limit.
	if err := configureDatabasePeerSocket(raw); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if config == nil {
		return raw, nil
	}
	config = config.Clone()
	if config.ServerName == "" {
		host, err := memberDialServerName(address)
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		config.ServerName = host
	}
	conn := tls.Client(raw, config)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

func memberDialServerName(address string) (string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return "", fmt.Errorf("database dial address %q has no host", address)
	}
	return host, nil
}

// directoryMembers lists the addresses the driver may ask for the leader. The
// database process keeps cluster.yaml current, so each lookup reads it again:
// a list captured at startup would name only this member on a fresh seed.
type directoryMembers struct {
	dir  string
	self client.NodeInfo
}

func (m directoryMembers) Get(context.Context) ([]client.NodeInfo, error) {
	raw, err := os.ReadFile(filepath.Join(m.dir, "cluster.yaml"))
	if err != nil {
		return []client.NodeInfo{m.self}, nil
	}
	return dialMembers(raw, m.self), nil
}

// Set is ignored: the database process owns cluster.yaml.
func (m directoryMembers) Set(context.Context, []client.NodeInfo) error { return nil }

func dialMembers(raw []byte, self client.NodeInfo) []client.NodeInfo {
	var listed []client.NodeInfo
	if err := yaml.Unmarshal(raw, &listed); err != nil || len(listed) == 0 {
		return []client.NodeInfo{self}
	}
	out := make([]client.NodeInfo, 0, len(listed)+1)
	found := false
	for _, node := range listed {
		if node.Address == "" {
			continue
		}
		if node.ID == self.ID || node.Address == self.Address {
			if node.ID == 0 {
				node.ID = self.ID
			}
			found = true
		}
		out = append(out, node)
	}
	if !found {
		out = append(out, self)
	}
	return out
}

func pause(ctx context.Context) error {
	timer := time.NewTimer(50 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
