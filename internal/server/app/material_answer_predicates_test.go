//go:build linux && dqlite

package app

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

type answerCase struct {
	name string
	err  error
}

type answerTimeout struct{}

func (answerTimeout) Error() string   { return "i/o timeout" }
func (answerTimeout) Timeout() bool   { return true }
func (answerTimeout) Temporary() bool { return true }

type answerEmptyJoin struct{}

func (answerEmptyJoin) Error() string   { return "empty join" }
func (answerEmptyJoin) Unwrap() []error { return nil }

func answerLeaves() []answerCase {
	op := func(op string, errno syscall.Errno) error {
		return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError(op, errno)}
	}
	leaves := []answerCase{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"io-deadline", os.ErrDeadlineExceeded},
		{"no-leader", dqlitedriver.ErrNoAvailableLeader},
		{"observation-timeout", store.ErrClusterMembershipObservationTimeout},
		{"bad-conn", driver.ErrBadConn},
		{"eof", io.EOF},
		{"unexpected-eof", io.ErrUnexpectedEOF},
		{"admission-rejected", store.ErrClusterAdmissionRejected},
		{"identity-conflict", store.ErrClusterIdentityConflict},
		{"maintenance-not-serving", store.ErrMaintenanceNotServing},
		{"maintenance-phase-changed", store.ErrMaintenancePhaseChanged},
		{"business-writes-closed", store.ErrBusinessWritesClosed},
		{"no-row", gorm.ErrRecordNotFound},
		{"no-majority", store.ErrMaterialManifestNoMajority},
		{"db-busy", dqlitedriver.Error{Code: dqlitedriver.ErrBusy, Message: "database is locked"}},
		{"db-failure", dqlitedriver.Error{Code: 3, Message: "key"}},
		{"net-reset", op("read", syscall.ECONNRESET)},
		{"net-refused", op("dial", syscall.ECONNREFUSED)},
		{"net-host-unreachable", op("dial", syscall.EHOSTUNREACH)},
		{"net-network-unreachable", op("dial", syscall.ENETUNREACH)},
		{"net-broken-pipe", op("write", syscall.EPIPE)},
		{"net-read-deadline", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}},
		{"net-timeout", answerTimeout{}},
		{"net-unknown-host", &net.DNSError{Err: "no such host", Name: "member", IsNotFound: true}},
		{"errno-reset", syscall.ECONNRESET},
		{"errno-timed-out", syscall.ETIMEDOUT},
		{"errno-no-entry", syscall.ENOENT},
		{"file-missing", &fs.PathError{Op: "open", Path: "policy", Err: syscall.ENOENT}},
		{"file-timed-out", &fs.PathError{Op: "read", Path: "policy", Err: syscall.ETIMEDOUT}},
		{"file-reset", &fs.PathError{Op: "read", Path: "policy", Err: syscall.ECONNRESET}},
		{"unclassified", errors.New("something else")},
		{"candidate-no-majority", errCandidateMaterialNoMajority},
		{"batch-unavailable", errRemoteMaterialBatchUnavailable},
		{"partitioned", &memberDatabasePartitioned{err: errors.New("receiver answered 503")}},
		{"receiver-unauthorized", &materialReceiverStatus{status: 503, text: "not ready", unauthorized: true}},
		{"receiver-refused", &materialReceiverStatus{status: 503, text: "changed"}},
	}
	// The same causes as a failed request to a peer.
	for _, leaf := range leaves[:len(leaves):len(leaves)] {
		leaves = append(leaves, answerCase{"request(" + leaf.name + ")", &url.Error{Op: "Post", URL: "https://member/material", Err: leaf.err}})
	}
	return leaves
}

func answerCases() []answerCase {
	leaves := answerLeaves()
	cases := []answerCase{{"nil", nil}, {"empty-join", answerEmptyJoin{}}, {"wrapped(empty-join)", fmt.Errorf("pass: %w", answerEmptyJoin{})}}
	for _, leaf := range leaves {
		cases = append(cases,
			leaf,
			answerCase{"wrapped(" + leaf.name + ")", fmt.Errorf("pass: %w", leaf.err)},
			answerCase{"wrapped(wrapped(" + leaf.name + "))", fmt.Errorf("outer: %w", fmt.Errorf("pass: %w", leaf.err))},
			answerCase{"join(" + leaf.name + ")", errors.Join(leaf.err)},
			answerCase{"wrapped(join(" + leaf.name + "))", fmt.Errorf("pass: %w", errors.Join(leaf.err))},
		)
	}
	for _, a := range leaves {
		for _, b := range leaves {
			pair := a.name + "+" + b.name
			cases = append(cases,
				answerCase{"join(" + pair + ")", errors.Join(a.err, b.err)},
				answerCase{"wrapped(join(" + pair + "))", fmt.Errorf("pass: %w", errors.Join(a.err, b.err))},
				answerCase{"join(wrapped " + pair + ")", errors.Join(fmt.Errorf("first: %w", a.err), fmt.Errorf("second: %w", b.err))},
				answerCase{"request(join(" + pair + "))", &url.Error{Op: "Post", URL: "https://member/material", Err: errors.Join(a.err, b.err)}},
			)
		}
	}
	return cases
}

// The store's policies and the two functions over them answer as the
// predicates they replace, for every cause bare, wrapped, joined with every
// other one, and as a failed request to a peer.
func TestAnswerPoliciesAnswerAsThePredicates(t *testing.T) {
	cases := answerCases()
	for _, pair := range []struct {
		name      string
		policy    func(error) bool
		predicate func(error) bool
	}{
		{"ProofUnanswered", store.ProofUnanswered.Unanswered, materialProofUnanswered},
		{"ProofReadUnanswered", store.ProofReadUnanswered.Unanswered, proofReadUnanswered},
		{"ManifestReadUnanswered", store.ManifestReadUnanswered.Unanswered, materialReadUnanswered},
		{"ManifestRecheckInterrupted", store.ManifestRecheckInterrupted.Unanswered, materialVerificationInterrupted},
		{"ReplicationUndecided", store.ReplicationUndecided.Unanswered, materialOutcomeUndecided},
		{"proofClusterLost", proofClusterLost, proofClusterUnavailable},
		{"peerUnreachable", peerUnreachable, unavailableMaterialPeer},
		{"GrantUndecided", store.GrantUndecided.Unanswered, store.ClusterGrantUndecided},
		{"GrantInterrupted", store.GrantInterrupted.Unanswered, store.ClusterGrantInterrupted},
	} {
		differences := 0
		for _, c := range cases {
			if got, want := pair.policy(c.err), pair.predicate(c.err); got != want {
				if differences++; differences <= 10 {
					t.Errorf("%s: %s: policy %t, predicate %t", pair.name, c.name, got, want)
				}
			}
		}
		if differences > 10 {
			t.Errorf("%s: %d differences in all", pair.name, differences)
		}
	}
	t.Logf("%d cases under each of nine pairs", len(cases))
}
