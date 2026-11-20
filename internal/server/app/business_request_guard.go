//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// businessMembershipGrant is the Store's live, majority-confirmed membership
// operation. Keeping this call injectable lets the request boundary be tested
// without a native cluster; production callers must use Store.GrantClusterMembership.
type businessMembershipGrant func(context.Context, store.ClusterIdentity, string) (uint64, []store.ClusterTransportMember, error)

func guardBusinessRequestsForStore(next http.Handler, st *store.Store, identity store.ClusterIdentity, memberID uint64, memberKey string, log *slog.Logger) http.Handler {
	if st == nil {
		return guardBusinessRequests(next, identity, memberID, memberKey, nil)
	}
	if log == nil {
		log = slog.Default()
	}
	grant := func(ctx context.Context, expected store.ClusterIdentity, key string) (uint64, []store.ClusterTransportMember, error) {
		started := time.Now()
		revision, members, err := st.GrantClusterMembership(ctx, expected, key)
		if err != nil {
			log.DebugContext(ctx, "business membership grant failed", "err_class", closedMemberErrorClass(err), "grant_ms", float64(time.Since(started))/float64(time.Millisecond))
		}
		return revision, members, err
	}
	return guardBusinessRequests(next, identity, memberID, memberKey, grant)
}

// Low-level errors may contain material paths or credentials. Only these
// classifications cross the diagnostic boundary; the original error is retained
// by the caller for its normal fail-closed behavior.
func closedMemberErrorClass(err error) string {
	var timeout net.Error
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, driver.ErrBadConn):
		return "bad_connection"
	case errors.Is(err, store.ErrClusterAdmissionRejected):
		return "admission_rejected"
	case errors.Is(err, store.ErrClusterIdentityConflict):
		return "identity_conflict"
	case errors.Is(err, store.ErrMaintenanceNotServing):
		return "maintenance_not_serving"
	case errors.Is(err, store.ErrBusinessWritesClosed):
		return "business_writes_closed"
	case store.ReplicatedLeaderUnavailable(err):
		return "no_leader"
	case errors.Is(err, store.ErrClusterMembershipObservationTimeout):
		return "observation_timeout"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection_closed"
	case errors.As(err, &timeout):
		return "network"
	case store.ReplicatedReadInterrupted(err):
		return "interrupted_response"
	default:
		return "other"
	}
}

// guardBusinessRequests checks current membership before starting any business
// request. GET can create terminal sessions and audit records, so only the
// process health probe bypasses the grant. It does not authorize the
// rest of an already-started request and must be wrapped around a business
// handler only after full candidate admission has succeeded.
func guardBusinessRequests(next http.Handler, identity store.ClusterIdentity, memberID uint64, memberKey string, grant businessMembershipGrant) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresBusinessGrant(r) {
			next.ServeHTTP(w, r)
			return
		}
		if grant == nil || memberID == 0 || memberKey == "" {
			businessGrantUnavailable(w)
			return
		}
		_, members, err := grant(r.Context(), identity, memberKey)
		if err != nil || len(members) < 2 {
			businessGrantUnavailable(w)
			return
		}
		for _, member := range members {
			if member.ID == memberID && member.KeyFingerprint == memberKey {
				next.ServeHTTP(w, r)
				return
			}
		}
		businessGrantUnavailable(w)
	})
}

func requiresBusinessGrant(r *http.Request) bool {
	return r.Method != http.MethodGet || r.URL.Path != "/healthz"
}

func businessGrantUnavailable(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "member business service is not ready", http.StatusServiceUnavailable)
}
