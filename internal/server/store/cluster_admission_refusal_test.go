package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// A refusal is a verdict against a member only when it names that member's
// key, or when the cluster identity conflicts. Everything else says nothing
// against it, however it is wrapped or joined.
func TestRefusesMemberAsksWhoseAdmissionItWas(t *testing.T) {
	own, other := &ClusterAdmissionRefusal{Key: "own"}, &ClusterAdmissionRefusal{Key: "other"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"own key", own, true},
		{"own key, wrapped", fmt.Errorf("grant: %w", own), true},
		{"another member's key", other, false},
		{"another member's key, wrapped", fmt.Errorf("grant: %w", other), false},
		{"a refusal that names no key", ErrClusterAdmissionRejected, false},
		{"a refusal that names no key, wrapped", fmt.Errorf("bind: %w", ErrClusterAdmissionRejected), false},
		{"identity conflict", ErrClusterIdentityConflict, true},
		{"identity conflict, wrapped", fmt.Errorf("grant: %w", ErrClusterIdentityConflict), true},
		{"maintenance not serving", fmt.Errorf("%w: draining at revision 3", ErrMaintenanceNotServing), false},
		{"business writes closed", ErrBusinessWritesClosed, false},
		{"maintenance phase changed", ErrMaintenancePhaseChanged, false},
		{"the writer guard's refusal", errors.Join(ErrBusinessWritesClosed, ErrClusterAdmissionRejected), false},
		{"own key joined to an expired deadline", errors.Join(context.DeadlineExceeded, own), true},
		{"own key joined under a wrapper", fmt.Errorf("proof: %w", errors.Join(context.DeadlineExceeded, own)), true},
		{"another key joined to an expired deadline", errors.Join(context.DeadlineExceeded, other), false},
		{"another key and own key", errors.Join(other, own), true},
		{"unclassified", errors.New("something else"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RefusesMember(tc.err, "own"); got != tc.want {
				t.Fatalf("RefusesMember=%t want %t", got, tc.want)
			}
		})
	}
	if RefusesMember(own, "") || RefusesMember(ErrClusterIdentityConflict, "") {
		t.Fatal("a member without a key was refused")
	}
}

// The named refusal is still the sentinel for every caller that does not
// ask whose it is, with the sentinel's text, and a refusal is named once.
func TestClusterAdmissionRefusalIsTheSentinel(t *testing.T) {
	named := refusedKey(ErrClusterAdmissionRejected, "k1")
	var refusal *ClusterAdmissionRefusal
	if !errors.As(named, &refusal) || refusal.Key != "k1" || !errors.Is(named, ErrClusterAdmissionRejected) || named.Error() != ErrClusterAdmissionRejected.Error() {
		t.Fatalf("named refusal: %#v", named)
	}
	if again := refusedKey(named, "k2"); again != named {
		t.Fatalf("a named refusal was renamed: %#v", again)
	}
	other := errors.New("database is locked")
	if got := refusedKey(other, "k1"); got != other {
		t.Fatalf("another error was changed: %v", got)
	}
	if refusedKey(nil, "k1") != nil {
		t.Fatal("nil became an error")
	}
	if errors.Is(named, ErrClusterIdentityConflict) {
		t.Fatal("an admission refusal reads as an identity conflict")
	}
}
