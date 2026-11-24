//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The causes only this package knows, next to the ones the store's policy
// decides: each is no answer or a finding for the two questions asked here.
func TestProofAndPeerAnswersWithTheCausesOnlyTheServerKnows(t *testing.T) {
	request := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://member/material", Err: err}
	}
	reset := &net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}
	partitioned := &memberDatabasePartitioned{err: errors.New("receiver answered 503")}
	unauthorized := &materialReceiverStatus{status: 503, text: "not ready", unauthorized: true}
	refused := &materialReceiverStatus{status: 503, text: "changed"}
	missing := &fs.PathError{Op: "open", Path: "policy", Err: syscall.ENOENT}
	for _, tc := range []struct {
		name        string
		err         error
		clusterLost bool
		unreachable bool
	}{
		{"nil", nil, false, false},
		{"no majority of candidate copies", errCandidateMaterialNoMajority, true, false},
		{"batch source unavailable", fmt.Errorf("batch: %w", errRemoteMaterialBatchUnavailable), true, false},
		{"canceled", context.Canceled, true, false},
		{"connection reset outside a request", reset, true, false},
		{"connection reset in a request", request(reset), true, true},
		{"request ended early", request(io.EOF), false, true},
		{"request cut inside the body", request(io.ErrUnexpectedEOF), true, true},
		{"request deadline", request(context.DeadlineExceeded), true, true},
		{"partitioned receiver", partitioned, false, true},
		{"receiver not ready, unlabelled bytes", unauthorized, false, true},
		{"receiver refused the bytes", refused, false, false},
		{"missing file", missing, false, false},
		{"missing file joined to a reset request", errors.Join(missing, request(reset)), false, false},
		{"partitioned joined to a refusal", errors.Join(partitioned, refused), false, false},
		{"partitioned joined to a reset request", errors.Join(partitioned, request(reset)), true, true},
		{"refusal by the cluster in a request", request(store.ErrClusterAdmissionRejected), false, false},
		{"unclassified", errors.New("something else"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := proofClusterLost(tc.err); got != tc.clusterLost {
				t.Errorf("proofClusterLost=%t want %t", got, tc.clusterLost)
			}
			if got := peerUnreachable(tc.err); got != tc.unreachable {
				t.Errorf("peerUnreachable=%t want %t", got, tc.unreachable)
			}
		})
	}
}
