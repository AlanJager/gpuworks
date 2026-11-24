//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"testing"
)

type dialTimeout struct{}

func (dialTimeout) Error() string   { return "i/o timeout" }
func (dialTimeout) Timeout() bool   { return true }
func (dialTimeout) Temporary() bool { return true }

func withMemberDatabaseDial(t *testing.T, dial func(context.Context, string) (net.Conn, error)) {
	t.Helper()
	previous := dialMemberDatabase
	dialMemberDatabase = dial
	t.Cleanup(func() { dialMemberDatabase = previous })
}

var (
	databaseTimesOut = func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: dialTimeout{}}
	}
	databaseRefuses = func(context.Context, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	databaseAccepts = func(context.Context, string) (net.Conn, error) {
		client, server := net.Pipe()
		_ = server.Close()
		return client, nil
	}
)

// A receiver that refuses with 503 while its database port times out from
// this member is partitioned away from this member's quorum: it contributes
// no copy and must not close this member's own admission.
func TestPartitionedReceiverRefusalIsAnUnavailablePeer(t *testing.T) {
	withMemberDatabaseDial(t, databaseTimesOut)
	for _, refusal := range []error{
		materialStatusError(statusResponse(http.StatusServiceUnavailable), []byte("material manifest is not current or complete")),
		&materialReceiverStatus{status: http.StatusServiceUnavailable, text: "manifest receiver rejected current bytes or identity: HTTP 503"},
	} {
		explained := explainMemberReachability(t.Context(), fmt.Errorf("replicate manifest to member 2: %w", refusal), "172.20.9.23:9001")
		if !peerUnreachable(explained) {
			t.Fatalf("partitioned 503 not unavailable: %v", explained)
		}
		probe := fmt.Errorf("probe material member 2: %w", explained)
		if materialReplicationFailureInvalidates(probe) {
			t.Fatalf("partitioned peer closed this member: %v", probe)
		}
		if explained.Error() != "replicate manifest to member 2: "+refusal.Error()+"; database 172.20.9.23:9001 timed out" {
			t.Fatalf("diagnostic text changed: %q", explained.Error())
		}
	}
}

// Every other combination keeps the existing fail-closed classification.
func TestReceiverRefusalWithoutPartitionEvidenceStillInvalidates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dial   func(context.Context, string) (net.Conn, error)
		status int
	}{
		{"503 database refused", databaseRefuses, http.StatusServiceUnavailable},
		{"503 database accepted", databaseAccepts, http.StatusServiceUnavailable},
		{"404 database timed out", databaseTimesOut, http.StatusNotFound},
		{"422 database timed out", databaseTimesOut, http.StatusUnprocessableEntity},
		{"409 database timed out", databaseTimesOut, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withMemberDatabaseDial(t, tc.dial)
			refusal := materialStatusError(statusResponse(tc.status), []byte("no"))
			explained := explainMemberReachability(t.Context(), refusal, "172.20.9.23:9001")
			if peerUnreachable(explained) {
				t.Fatalf("treated as unavailable: %v", explained)
			}
			if !materialReplicationFailureInvalidates(explained) {
				t.Fatalf("no longer invalidates: %v", explained)
			}
		})
	}
}

// Identity headers that differ are a proof failure even when the database
// port times out: only the 503 status carries the partition reading.
func TestManifestIdentityMismatchIsNeverAPartition(t *testing.T) {
	withMemberDatabaseDial(t, databaseTimesOut)
	mismatch := &materialReceiverStatus{status: http.StatusNoContent, text: "manifest receiver rejected current bytes or identity: HTTP 204"}
	explained := explainMemberReachability(t.Context(), mismatch, "172.20.9.23:9001")
	if peerUnreachable(explained) || !materialReplicationFailureInvalidates(explained) {
		t.Fatalf("identity mismatch softened: %v", explained)
	}
}

// A partitioned peer joined with an integrity failure from elsewhere still
// closes: the join rule is unchanged.
func TestPartitionedPeerDoesNotHideAJoinedIntegrityFailure(t *testing.T) {
	withMemberDatabaseDial(t, databaseTimesOut)
	partitioned := explainMemberReachability(t.Context(), materialStatusError(statusResponse(http.StatusServiceUnavailable), nil), "172.20.9.23:9001")
	joined := errors.Join(partitioned, errors.New("material digest differs"))
	if !materialReplicationFailureInvalidates(joined) {
		t.Fatalf("joined integrity failure hidden: %v", joined)
	}
}

func TestMaterialReceiverStatusKeepsExactText(t *testing.T) {
	if got := materialStatusError(statusResponse(http.StatusServiceUnavailable), nil).Error(); got != "material receiver rejected transfer: HTTP 503" {
		t.Fatalf("text=%q", got)
	}
}

func statusResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}}
}
