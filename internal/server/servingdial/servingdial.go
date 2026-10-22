// Package servingdial reaches a node's serving forwarder over the node's
// serving network (see proto.ServingForwardMagic). The gateway uses it for
// every request to an instance on such a node, and the controller uses it to
// check the network before starting an instance there. It is a leaf.
package servingdial

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Timeout bounds reaching the forwarder and its handshake.
const Timeout = 2 * time.Second

// DialFunc opens a TCP connection.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dialer dials serving forwarders; the routing table picks the local
// address, which is the one on the interface toward the serving network.
func Dialer() DialFunc {
	return (&net.Dialer{}).DialContext
}

// handshake opens TLS to the forwarder at ip, pinning the key derived from
// the node token.
func handshake(ctx context.Context, dial DialFunc, ip, nodeToken string) (*tls.Conn, error) {
	raw, err := dial(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(proto.ServingForwardPort)))
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, proto.ServingForwardClientTLS(nodeToken))
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// Probe proves this server reaches the node's forwarder over the serving
// network, in the direction serving traffic flows, and that it is the node's.
func Probe(ctx context.Context, dial DialFunc, ip, nodeToken string) error {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	conn, err := handshake(ctx, dial, ip, nodeToken)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Connect returns a connection the forwarder has spliced to the instance.
func Connect(ctx context.Context, dial DialFunc, ip, nodeToken, instanceUUID string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	conn, err := handshake(ctx, dial, ip, nodeToken)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "%s %s %s\n", proto.ServingForwardMagic, instanceUUID, proto.ServingForwardToken(nodeToken, instanceUUID)); err != nil {
		return fail(err)
	}
	reply, err := readReplyLine(conn)
	if err != nil {
		return fail(err)
	}
	if reply != "OK" {
		return fail(errors.New("forwarder: " + strings.TrimPrefix(reply, "ERR ")))
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// readReplyLine reads byte by byte so no upstream byte after the reply is
// consumed before the HTTP transport takes over the connection.
func readReplyLine(conn net.Conn) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for b.Len() < 256 {
		if _, err := conn.Read(buf); err != nil {
			return "", err
		}
		if buf[0] == '\n' {
			return b.String(), nil
		}
		b.WriteByte(buf[0])
	}
	return "", errors.New("forwarder reply too long")
}
