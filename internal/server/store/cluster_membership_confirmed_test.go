package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestConfirmedClusterMembershipFollowsSuccessfulGrantsOnly(t *testing.T) {
	s := &Store{}
	if _, ok := s.ConfirmedClusterMembership(time.Hour); ok {
		t.Fatal("a store that never took a grant reported a confirmed membership")
	}
	members := []ClusterTransportMember{{ID: 1, Address: "127.0.0.1:9001", KeyFingerprint: "a"}, {ID: 2, Address: "127.0.0.2:9001", KeyFingerprint: "b"}}
	var fail error
	s.grantClusterMembership = func(context.Context, ClusterIdentity, string) (uint64, []ClusterTransportMember, error) {
		if fail != nil {
			return 0, nil, fail
		}
		return 7, members, nil
	}
	s.grantClusterMembershipKeys = func(context.Context, ClusterIdentity, []string) (uint64, []ClusterTransportMember, error) {
		if fail != nil {
			return 0, nil, fail
		}
		return 8, members[:1], nil
	}
	if _, _, err := s.GrantClusterMembership(t.Context(), ClusterIdentity{}, "a"); err != nil {
		t.Fatal(err)
	}
	got, ok := s.ConfirmedClusterMembership(time.Hour)
	if !ok || len(got) != 2 || got[0] != members[0] || got[1] != members[1] {
		t.Fatalf("confirmed membership after a grant = %v %v", got, ok)
	}
	// The caller's copy is its own: changing it changes no later answer.
	got[0].KeyFingerprint = "changed"
	if again, _ := s.ConfirmedClusterMembership(time.Hour); again[0].KeyFingerprint != "a" {
		t.Fatal("a caller changed the confirmed membership")
	}
	// A grant that fails, whether storage did not answer or the database
	// refused, confirms nothing and leaves the last confirmation in place.
	for _, fail = range []error{context.DeadlineExceeded, ErrClusterAdmissionRejected} {
		if _, _, err := s.GrantClusterMembership(t.Context(), ClusterIdentity{}, "a"); !errors.Is(err, fail) {
			t.Fatalf("grant error = %v, want %v", err, fail)
		}
		if got, ok := s.ConfirmedClusterMembership(time.Hour); !ok || len(got) != 2 {
			t.Fatalf("a failed grant (%v) replaced the confirmed membership: %v %v", fail, got, ok)
		}
	}
	fail = nil
	// The multi-key grant confirms as well, and the newest confirmation wins.
	if _, _, err := s.GrantClusterMembershipKeys(t.Context(), ClusterIdentity{}, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.ConfirmedClusterMembership(time.Hour); !ok || len(got) != 1 {
		t.Fatalf("confirmed membership after a multi-key grant = %v %v", got, ok)
	}
	// Age is the caller's bound: a confirmation older than it is not used.
	s.confirmedMembership.Load().at = time.Now().Add(-time.Minute)
	if _, ok := s.ConfirmedClusterMembership(30 * time.Second); ok {
		t.Fatal("a confirmation older than the bound was used")
	}
	if _, ok := s.ConfirmedClusterMembership(2 * time.Minute); !ok {
		t.Fatal("a confirmation inside the bound was not used")
	}
	if _, ok := s.ConfirmedClusterMembership(0); ok {
		t.Fatal("a zero bound used a confirmation")
	}
	// The member's own invalidation drops the list; the next caller takes a grant.
	s.ForgetConfirmedClusterMembership()
	if _, ok := s.ConfirmedClusterMembership(time.Hour); ok {
		t.Fatal("a forgotten membership was still reported as confirmed")
	}
	// The age is measured on the monotonic clock: a wall clock stepped back
	// by an hour does not make an old confirmation young.
	if _, _, err := s.GrantClusterMembership(t.Context(), ClusterIdentity{}, "a"); err != nil {
		t.Fatal(err)
	}
	if at := s.confirmedMembership.Load().at; at.Equal(at.Round(0)) && at == at.Round(0) {
		t.Fatal("the confirmation time carries no monotonic reading")
	}
}
