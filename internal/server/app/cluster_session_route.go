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
	confirmed := a.store.ConfirmedClusterMembership
	grant := func(ctx context.Context) ([]store.ClusterTransportMember, error) {
		_, members, err := a.store.GrantClusterMembership(ctx, expected, localKey)
		return members, err
	}
	authorize := func(ctx context.Context, cert *x509.Certificate) error {
		key, err := bundle.trust.VerifyMember(cert, time.Now().UTC())
		if err != nil {
			return err
		}
		pick := func(members []store.ClusterTransportMember) string {
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
				return ""
			}
			return key
		}
		if admitted, err := memberRouteKey(ctx, confirmed, grant, pick); err != nil {
			return err
		} else if admitted == "" {
			return errors.New("member session peer is not live and admitted")
		}
		return nil
	}
	server := bundle.tls.Clone()
	server.MinVersion = tls.VersionTLS13
	server.ClientAuth = tls.RequireAndVerifyClientCert
	server.SessionTicketsDisabled = true
	peerTLS := func(ctx context.Context, id uint64, address string) (*tls.Config, error) {
		pick := func(members []store.ClusterTransportMember) string {
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
			if !local {
				return ""
			}
			return peerKey
		}
		peerKey, err := memberRouteKey(ctx, confirmed, grant, pick)
		if err != nil {
			return nil, err
		}
		if peerKey == "" {
			return nil, errors.New("member session target is not live and admitted")
		}
		client := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{bundle.tls.Certificates[0]}, SessionTicketsDisabled: true,
			// The confirmed membership pins the target address and public key. The
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

// memberRouteMembershipAge is how old a confirmed member list may be and
// still admit a connection between members: the lifetime of the proof that
// keeps this member's own business open, and renewed by that proof.
const memberRouteMembershipAge = materialEligibilityWindow

// memberRouteUnansweredAge is how old that list may be when a fresh grant
// brought no answer: as long as a member's own business stays open without
// an answer, by the same constant.
//
// What it costs: a member revoked during a storage outage is accepted by its
// peers for up to this long. The member that commits the revocation drops
// its list at once, and so does a member whose own admission closes.
const memberRouteUnansweredAge = materialUnconfirmedLimit

// memberRouteKey picks a member key for one connection between members. It
// answers from the membership this process last confirmed, so that opening a
// connection does not wait for a replicated write. Only when there is no
// confirmed list young enough, or that list does not name the member, does
// it take a fresh grant: a member admitted since the last confirmation is
// found that way, and one removed since then is still accepted until the
// next confirmation.
//
// A grant that brings no answer (storage stalled, no leader) is not a
// refusal: the last confirmed list then stands in for it up to
// memberRouteUnansweredAge.
func memberRouteKey(ctx context.Context, confirmed func(time.Duration) ([]store.ClusterTransportMember, bool), grant func(context.Context) ([]store.ClusterTransportMember, error), pick func([]store.ClusterTransportMember) string) (string, error) {
	if members, ok := confirmed(memberRouteMembershipAge); ok {
		if key := pick(members); key != "" {
			return key, nil
		}
	}
	members, err := grant(ctx)
	if err != nil {
		if store.ClusterGrantUndecided(err) {
			if members, ok := confirmed(memberRouteUnansweredAge); ok {
				if key := pick(members); key != "" {
					return key, nil
				}
			}
		}
		return "", err
	}
	return pick(members), nil
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
