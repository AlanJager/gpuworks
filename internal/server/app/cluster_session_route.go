//go:build linux && dqlite

package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// memberSessionHub is one member's end of the member route: it accepts what
// other members hand over and dials them. Without it each member serves only
// the agents connected to its own process.
type memberSessionHub interface {
	SetMemberIdentity(memberID uint64, key []byte)
	SetMemberSecureTransport(memberID uint64, server *tls.Config, peerTLS func(context.Context, uint64, string) (*tls.Config, error), authorize func(context.Context, *x509.Certificate) error) error
	SetMemberRoute(memberID uint64, addr string)
	ServeMemberSessions(ln net.Listener) error
	CloseMemberSessions(ctx context.Context) error
}

// memberSessions is what serves this member's end of the member route: the
// forwarder of its business handler, or nil when the App has none.
func (a *App) memberSessions() memberSessionHub {
	if a == nil {
		return nil
	}
	// A member that was never enrolled in a cluster has no certificate for
	// the route and nobody to hand a request to.
	if runtime, ok := a.ha.(*memberRuntime); ok && runtime.forward != nil && runtime.bundle != nil {
		return runtime.forward
	}
	return nil
}

// routeClusterMemberSessions starts session routing when this process is a
// cluster member. An ordinary store has no route, so the hub stays local and
// the business entry is left untouched. The returned stop closes the route;
// it is nil when nothing was started.
func (a *App) routeClusterMemberSessions(ctx context.Context, sessions memberSessionHub) (stop func() error, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.store == nil {
		return nil, nil
	}
	route, ok := a.store.ClusterSessionRoute()
	if !ok {
		return nil, nil
	}
	if sessions == nil {
		// Without a router each member serves the agents that dialed it: a
		// request that needs another member's agent fails on this one.
		a.log.Warn("member sessions are not routed between members; an agent is served only by the member it dials", "member", route.ID)
		return nil, nil
	}
	listen := strings.TrimSpace(route.Listen)
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	if route.Secure {
		if err := a.configureSecureSessionRoute(route, sessions); err != nil {
			_ = ln.Close()
			return nil, err
		}
	} else {
		sessions.SetMemberIdentity(route.ID, route.Key)
	}
	sessions.SetMemberRoute(route.ID, ln.Addr().String())
	for _, peer := range route.Peers {
		if peer.ID == 0 || peer.ID == route.ID || strings.TrimSpace(peer.Address) == "" {
			continue
		}
		sessions.SetMemberRoute(peer.ID, peer.Address)
	}
	go func() {
		if err := sessions.ServeMemberSessions(ln); err != nil && a.log != nil {
			a.log.Warn("member session listener stopped", "err", err)
		}
	}()
	return func() error {
		shutdownErr := sessions.CloseMemberSessions(context.Background())
		listenerErr := ln.Close()
		if errors.Is(listenerErr, net.ErrClosed) {
			listenerErr = nil
		}
		return errors.Join(shutdownErr, listenerErr)
	}, nil
}

func (a *App) configureSecureSessionRoute(route store.ClusterSessionRoute, sessions memberSessionHub) error {
	bundle, err := readEnrollmentBundle(a.cfg.DataDir, false)
	if err != nil || bundle == nil || bundle.tls == nil || bundle.trust == nil || len(bundle.tls.Certificates) != 1 {
		return errors.New("native session route requires local enrollment identity")
	}
	localKey, err := bundle.trust.VerifyMember(bundle.tls.Certificates[0].Leaf, time.Now().UTC())
	if err != nil {
		return err
	}
	expected := store.ClusterIdentity{ClusterID: bundle.info.ClusterID, CAFingerprint: bundle.info.CAFingerprint, Protocol: bundle.info.Protocol}
	authorize := func(ctx context.Context, cert *x509.Certificate) error {
		key, err := bundle.trust.VerifyMember(cert, time.Now().UTC())
		if err != nil {
			return err
		}
		_, members, err := a.store.GrantClusterMembership(ctx, expected, localKey)
		if err != nil {
			return err
		}
		local, peer := false, false
		for _, member := range members {
			if member.ID == route.ID && member.KeyFingerprint == localKey {
				local = true
			}
			if member.KeyFingerprint == key {
				peer = true
			}
		}
		if !local || !peer {
			return errors.New("member session peer is not live and admitted")
		}
		return nil
	}
	server := bundle.tls.Clone()
	server.MinVersion = tls.VersionTLS13
	server.ClientAuth = tls.RequireAndVerifyClientCert
	server.SessionTicketsDisabled = true
	peerTLS := func(ctx context.Context, id uint64, address string) (*tls.Config, error) {
		_, members, err := a.store.GrantClusterMembership(ctx, expected, localKey)
		if err != nil {
			return nil, err
		}
		var peerKey string
		local := false
		for _, member := range members {
			if member.ID == route.ID && member.KeyFingerprint == localKey {
				local = true
			}
			if member.ID == id && sessionMemberAddress(member.Address) == address {
				peerKey = member.KeyFingerprint
			}
		}
		if !local || peerKey == "" {
			return nil, errors.New("member session target is not live and admitted")
		}
		client := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{bundle.tls.Certificates[0]}, SessionTicketsDisabled: true,
			// The quorum grant pins the target address and public key. The
			// enrollment hostname can differ from the Raft address, so validate
			// the pinned member certificate instead of a DNS name.
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("member session peer certificate missing")
				}
				key, err := bundle.trust.VerifyMember(state.PeerCertificates[0], time.Now().UTC())
				if err != nil {
					return err
				}
				if key != peerKey {
					return errors.New("member session peer key differs from current membership")
				}
				return nil
			},
		}
		return client, nil
	}
	return sessions.SetMemberSecureTransport(route.ID, server, peerTLS, authorize)
}

func sessionMemberAddress(databaseAddress string) string {
	host, port, err := net.SplitHostPort(databaseAddress)
	if err != nil {
		return ""
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number >= 65535 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(number+1))
}
