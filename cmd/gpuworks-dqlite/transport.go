//go:build linux && cgo && dqlite

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

// peerLivenessTimeout clears a blackholed peer session with headroom inside
// the server's five-second membership grant. It applies to SQL and Raft
// connections; acknowledged healthy connections have no age limit.
const peerLivenessTimeout = 3 * time.Second

// filePolicy authorizes each handshake from the policy file as it is then.
// The server owns that file's lease and publishes changes to it; this process
// only reads. Anything it cannot read and bind to the pinned authority, and
// any revision older than one it has already honored, refuses the peer.
type filePolicy struct {
	path      string
	clusterID string
	authority string

	mu       sync.Mutex
	revision uint64
}

func (p *filePolicy) authorize(key string) error {
	policy, err := identity.LoadTransportPolicy(p.path)
	if err != nil {
		return errors.New("transport policy is unavailable: " + err.Error())
	}
	authorize, err := policy.Authorizer(p.clusterID, p.authority)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if policy.Revision < p.revision {
		return errors.New("transport policy revision regressed")
	}
	if err := authorize(key); err != nil {
		return err
	}
	p.revision = policy.Revision
	return nil
}

// loadTransportTLS binds the member certificate, the pinned authority and the
// policy file together before the node listens. The local member itself must
// be authorized, so a revoked member cannot start its transport at all.
func loadTransportTLS(certPath, keyPath, caPath, policyPath string) (*tls.Config, *tls.Config, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, nil, err
	}
	policy, err := identity.LoadTransportPolicy(policyPath)
	if err != nil {
		return nil, nil, errors.New("transport policy: " + err.Error())
	}
	now := time.Now().UTC()
	trust, err := identity.NewTrust(caPEM, policy.ClusterID, policy.CAFingerprint, now)
	if err != nil {
		return nil, nil, err
	}
	owner := &filePolicy{path: policyPath, clusterID: policy.ClusterID, authority: policy.CAFingerprint, revision: policy.Revision}
	listen, dial, err := trust.TLSConfigs(certPEM, keyPEM, now, owner.authorize)
	if err != nil {
		return nil, nil, err
	}
	local, err := x509.ParseCertificate(listen.Certificates[0].Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	fingerprint, err := trust.VerifyMember(local, now)
	if err != nil {
		return nil, nil, err
	}
	if err := owner.authorize(fingerprint); err != nil {
		return nil, nil, errors.New("local member is not authorized by the transport policy")
	}
	return withPeerLiveness(listen), dial, nil
}

func withPeerLiveness(config *tls.Config) *tls.Config {
	listen := config.Clone()
	previous := listen.GetConfigForClient
	listen.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		var selected *tls.Config
		if previous != nil {
			var err error
			selected, err = previous(hello)
			if err != nil {
				return selected, err
			}
		}
		// go-dqlite sets its own socket defaults before the handshake starts.
		// The hello carries that same accepted TCP connection, so these
		// settings replace its thirty-second policy.
		if err := configurePeerSocket(hello.Conn); err != nil {
			return nil, err
		}
		return selected, nil
	}
	return listen
}

func configurePeerSocket(conn net.Conn) error {
	tcp, ok := conn.(*net.TCPConn)
	if !ok || tcp == nil {
		return errors.New("database TLS requires a TCP connection")
	}
	if err := tcp.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: time.Second, Interval: time.Second, Count: int(peerLivenessTimeout / time.Second)}); err != nil {
		return err
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	controlErr := raw.Control(func(fd uintptr) {
		socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(peerLivenessTimeout/time.Millisecond))
	})
	return errors.Join(controlErr, socketErr)
}
