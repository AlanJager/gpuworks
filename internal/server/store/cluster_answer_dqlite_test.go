//go:build linux && dqlite

package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
	"gorm.io/gorm"
)

var updateAnswerTable = flag.Bool("update-answer-table", false, "rewrite testdata/answer_policies.txt from the policies")

// answerPolicies is the order of the columns in the tables below.
var answerPolicies = []struct {
	name   string
	policy AnswerPolicy
}{
	{"GrantInterrupted", GrantInterrupted},
	{"GrantUndecided", GrantUndecided},
	{"ProofUnanswered", ProofUnanswered},
	{"ProofReadUnanswered", ProofReadUnanswered},
	{"ProofClusterLost", ProofClusterLost},
	{"ManifestReadUnanswered", ManifestReadUnanswered},
	{"ManifestRecheckInterrupted", ManifestRecheckInterrupted},
	{"ReplicationUndecided", ReplicationUndecided},
	{"PeerUnreachable", PeerUnreachable},
}

type answerCase struct {
	name string
	err  error
}

// timeoutError is a network error that timed out and says nothing else.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// emptyJoin is a join with no branches.
type emptyJoin struct{}

func (emptyJoin) Error() string   { return "empty join" }
func (emptyJoin) Unwrap() []error { return nil }

// answerLeaves is every sentinel and typed error a policy tells apart, and
// the ones next to them it must not confuse them with.
func answerLeaves() []answerCase {
	op := func(op string, errno syscall.Errno) error {
		return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError(op, errno)}
	}
	return []answerCase{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"io-deadline", os.ErrDeadlineExceeded},
		{"no-leader", dqlitedriver.ErrNoAvailableLeader},
		{"observation-timeout", ErrClusterMembershipObservationTimeout},
		{"bad-conn", driver.ErrBadConn},
		{"eof", io.EOF},
		{"unexpected-eof", io.ErrUnexpectedEOF},
		{"membership-changed", errClusterMembershipChanged},
		{"admission-rejected", ErrClusterAdmissionRejected},
		{"identity-conflict", ErrClusterIdentityConflict},
		{"maintenance-not-serving", ErrMaintenanceNotServing},
		{"maintenance-phase-changed", ErrMaintenancePhaseChanged},
		{"business-writes-closed", ErrBusinessWritesClosed},
		{"no-row", gorm.ErrRecordNotFound},
		{"no-majority", ErrMaterialManifestNoMajority},
		{"db-busy", dqlitedriver.Error{Code: dqlitedriver.ErrBusy, Message: "database is locked"}},
		{"db-busy-extended", dqlitedriver.Error{Code: dqlitedriver.ErrBusy | 2<<8, Message: "database is locked"}},
		{"db-failure", dqlitedriver.Error{Code: 3, Message: "key"}},
		{"net-reset", op("read", syscall.ECONNRESET)},
		{"net-refused", op("dial", syscall.ECONNREFUSED)},
		{"net-host-unreachable", op("dial", syscall.EHOSTUNREACH)},
		{"net-network-unreachable", op("dial", syscall.ENETUNREACH)},
		{"net-broken-pipe", op("write", syscall.EPIPE)},
		{"net-read-deadline", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}},
		{"net-timeout", timeoutError{}},
		{"net-unknown-host", &net.DNSError{Err: "no such host", Name: "member", IsNotFound: true}},
		{"errno-reset", syscall.ECONNRESET},
		{"errno-timed-out", syscall.ETIMEDOUT},
		{"errno-again", syscall.EAGAIN},
		{"errno-no-entry", syscall.ENOENT},
		{"file-missing", &fs.PathError{Op: "open", Path: "policy", Err: syscall.ENOENT}},
		{"file-timed-out", &fs.PathError{Op: "read", Path: "policy", Err: syscall.ETIMEDOUT}},
		{"file-disk-full", &fs.PathError{Op: "write", Path: "policy", Err: syscall.ENOSPC}},
		{"file-reset", &fs.PathError{Op: "read", Path: "policy", Err: syscall.ECONNRESET}},
		{"unclassified", errors.New("something else")},
		{"request(net-reset)", request(op("read", syscall.ECONNRESET))},
		{"request(net-timeout)", request(timeoutError{})},
		{"request(eof)", request(io.EOF)},
		{"request(deadline)", request(context.DeadlineExceeded)},
		{"request(net-broken-pipe)", request(op("write", syscall.EPIPE))},
		{"request(admission-rejected)", request(ErrClusterAdmissionRejected)},
		{"request(unclassified)", request(errors.New("certificate is not the member's"))},
	}
}

// request is err as the failure of an HTTP request to another member.
func request(err error) error {
	return &url.Error{Op: "Post", URL: "https://member/material", Err: err}
}

// answerSingles is each leaf bare, wrapped, wrapped twice, as the only branch
// of a join and as that join wrapped, with nil and an empty join.
func answerSingles() []answerCase {
	cases := []answerCase{{"nil", nil}, {"empty-join", emptyJoin{}}, {"wrapped(empty-join)", fmt.Errorf("pass: %w", emptyJoin{})}}
	for _, leaf := range answerLeaves() {
		cases = append(cases,
			leaf,
			answerCase{"wrapped(" + leaf.name + ")", fmt.Errorf("pass: %w", leaf.err)},
			answerCase{"wrapped(wrapped(" + leaf.name + "))", fmt.Errorf("outer: %w", fmt.Errorf("pass: %w", leaf.err))},
			answerCase{"join(" + leaf.name + ")", errors.Join(leaf.err)},
			answerCase{"wrapped(join(" + leaf.name + "))", fmt.Errorf("pass: %w", errors.Join(leaf.err))},
		)
	}
	return cases
}

// answerPairs is every ordered pair of leaves in the three shapes a second
// cause arrives in. The order matters: the first network error found speaks
// for the rest.
func answerPairs() []answerCase {
	leaves := answerLeaves()
	var cases []answerCase
	for _, a := range leaves {
		for _, b := range leaves {
			pair := a.name + "+" + b.name
			cases = append(cases,
				answerCase{"join(" + pair + ")", errors.Join(a.err, b.err)},
				answerCase{"wrapped(join(" + pair + "))", fmt.Errorf("pass: %w", errors.Join(a.err, b.err))},
				answerCase{"join(wrapped " + pair + ")", errors.Join(fmt.Errorf("first: %w", a.err), fmt.Errorf("second: %w", b.err))},
			)
		}
	}
	return cases
}

// answerRow is one letter per policy, in answerPolicies' order: U for no
// answer, a dot for a finding.
func answerRow(err error) string {
	var row strings.Builder
	for _, column := range answerPolicies {
		if column.policy.Unanswered(err) {
			row.WriteByte('U')
		} else {
			row.WriteByte('.')
		}
	}
	return row.String()
}

// Every sentinel under every policy, bare, wrapped and joined with every
// other one. The table is a file because it has some six thousand rows; a
// change to a policy or to the classifier shows as a diff of it.
func TestAnswerPolicyTable(t *testing.T) {
	var table strings.Builder
	table.WriteString("# U = no answer, . = a finding. Columns:")
	for _, column := range answerPolicies {
		table.WriteString(" " + column.name)
	}
	table.WriteString("\n")
	for _, c := range append(answerSingles(), answerPairs()...) {
		fmt.Fprintf(&table, "%s %s\n", answerRow(c.err), c.name)
	}
	const path = "testdata/answer_policies.txt"
	if *updateAnswerTable {
		if err := os.WriteFile(path, []byte(table.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if table.String() == string(want) {
		return
	}
	got, wanted := strings.Split(table.String(), "\n"), strings.Split(string(want), "\n")
	shown := 0
	for i := 0; i < len(got) && i < len(wanted) && shown < 20; i++ {
		if got[i] != wanted[i] {
			t.Errorf("row %d:\n got %s\nwant %s", i+1, got[i], wanted[i])
			shown++
		}
	}
	if len(got) != len(wanted) {
		t.Errorf("%d rows, want %d", len(got), len(wanted))
	}
}
