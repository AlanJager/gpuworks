//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"gpuworks.ai/gpuworks/internal/server/cluster/identity"
)

const materialPeerIdleTimeout = 15 * time.Second

// memberMaterialClient binds an HTTPS origin to the member key in a fresh
// membership grant. CA and hostname checks alone would also trust another
// member of this cluster at the same endpoint.
func memberMaterialClient(bundle *enrollmentBundle, origin, expectedKey string) (*http.Client, error) {
	if bundle == nil || bundle.tls == nil || len(bundle.tls.Certificates) != 1 || bundle.trust == nil || expectedKey == "" {
		return nil, errors.New("material peer requires local member identity and target key")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("material peer requires an exact HTTPS origin")
	}
	config, err := identity.EnrollmentTLSConfig(bundle.info.ClusterID, bundle.info.CAFingerprint, u.Hostname())
	if err != nil {
		return nil, err
	}
	config.Certificates = []tls.Certificate{bundle.tls.Certificates[0]}
	verifyOrigin := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if err := verifyOrigin(state); err != nil {
			return err
		}
		if len(state.PeerCertificates) == 0 {
			return errors.New("material peer did not present a member certificate")
		}
		key, err := bundle.trust.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
		if err != nil {
			return err
		}
		if key != expectedKey {
			return errors.New("material peer certificate differs from current membership")
		}
		return nil
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: config,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &materialProgressConn{Conn: conn, idle: materialPeerIdleTimeout}, nil
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: materialPeerIdleTimeout,
	}}, nil
}

// Traffic in either direction renews the socket's shared idle deadline. The
// HTTP reader can already be waiting for a reply during a long upload; write
// progress must keep that pending read alive until the receiver can respond.
type materialProgressConn struct {
	net.Conn
	idle time.Duration
}

func (c *materialProgressConn) Read(p []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		if deadlineErr := c.SetDeadline(time.Now().Add(c.idle)); err == nil {
			err = deadlineErr
		}
	}
	return n, err
}

func (c *materialProgressConn) Write(p []byte) (int, error) {
	if err := c.SetDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if n > 0 {
		if deadlineErr := c.SetDeadline(time.Now().Add(c.idle)); err == nil {
			err = deadlineErr
		}
	}
	return n, err
}
