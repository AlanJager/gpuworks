//go:build linux && dqlite

package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// ProbeMember authenticates exactly one supplied Raft endpoint and exchanges a
// real dqlite Leader request. It does not follow the returned leader, open SQL,
// change membership, or assert cluster readiness/quorum. The caller must supply
// an independently authorized policy and the expected endpoint public key.
func ProbeMember(ctx context.Context, address, expectedKey string, trust *identity.Trust, certificate, key []byte, policy identity.TransportPolicy) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	defer func() {
		if err := ctx.Err(); err != nil {
			resultErr = errors.Join(err, resultErr)
		}
	}()
	if err := probeMemberAddress(address); err != nil {
		return err
	}
	if trust == nil {
		return errors.New("member probe requires pinned cluster trust")
	}
	authorize, err := trust.PolicyAuthorizer(policy)
	if err != nil {
		return err
	}
	if err := authorize(expectedKey); err != nil {
		return err
	}
	_, dialTLS, err := trust.TLSConfigs(certificate, key, time.Now().UTC(), authorize)
	if err != nil {
		return err
	}
	local, err := x509.ParseCertificate(dialTLS.Certificates[0].Certificate[0])
	if err != nil {
		return err
	}
	fingerprint, err := trust.VerifyMember(local, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := authorize(fingerprint); err != nil {
		return err
	}
	verify := dialTLS.VerifyConnection
	dialTLS.VerifyConnection = func(state tls.ConnectionState) error {
		if err := verify(state); err != nil {
			return err
		}
		remote, err := trust.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			return err
		}
		if remote != expectedKey {
			return errors.New("member probe endpoint public key differs from expected member")
		}
		return nil
	}
	var stopCancellation func() bool
	defer func() {
		if stopCancellation != nil {
			stopCancellation()
		}
	}()
	direct := func(callCtx context.Context, target string) (net.Conn, error) {
		if target != address {
			return nil, errors.New("member probe refuses a redirected endpoint")
		}
		connection, err := (&tls.Dialer{Config: dialTLS}).DialContext(callCtx, "tcp", address)
		if err != nil {
			return nil, err
		}
		deadline, _ := ctx.Deadline()
		if err := connection.SetDeadline(deadline); err != nil {
			_ = connection.Close()
			return nil, err
		}
		stopCancellation = context.AfterFunc(ctx, func() { _ = connection.Close() })
		return &probeReplyConn{Conn: connection}, nil
	}
	connection, err := client.New(ctx, address, client.WithDialFunc(direct))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	_, err = connection.Leader(ctx)
	if err != nil {
		return err
	}
	return ctx.Err()
}

func probeMemberAddress(address string) error {
	normalized, err := identity.NormalizeMemberAddress(address)
	if err != nil {
		return err
	}
	if normalized != address {
		return errors.New("member probe address is not canonical")
	}
	return nil
}

// probeReplyLimit bounds one reply to a probe. A leader reply is a member id
// and an address.
const probeReplyLimit = 64 << 10

// probeReplyConn refuses a reply whose header announces more than a probe can
// receive. The client allocates the announced size before reading the body, so
// an endpoint that is not dqlite could otherwise make this process allocate
// and clear gigabytes: the first four bytes of "HTTP/1.1" read as a word count
// announce a body of about ten gigabytes.
type probeReplyConn struct {
	net.Conn
	header [8]byte
	filled int
	body   uint64
}

func (c *probeReplyConn) Read(p []byte) (int, error) {
	// Never hand over more than the rest of the current header or body, so
	// every header is checked before the client can act on it.
	limit := uint64(len(c.header) - c.filled)
	if c.body > 0 {
		limit = c.body
	}
	if uint64(len(p)) > limit {
		p = p[:limit]
	}
	n, err := c.Conn.Read(p)
	if c.body > 0 {
		c.body -= uint64(n)
		return n, err
	}
	c.filled += copy(c.header[c.filled:], p[:n])
	if c.filled == len(c.header) {
		c.filled = 0
		c.body = uint64(binary.LittleEndian.Uint32(c.header[:4])) * 8
		if c.body > probeReplyLimit {
			_ = c.Conn.Close()
			return 0, errors.New("member probe endpoint does not speak the database protocol")
		}
	}
	return n, err
}
