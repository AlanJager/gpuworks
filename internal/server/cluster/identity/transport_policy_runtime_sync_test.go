package identity

import (
	"errors"
	"slices"
	"testing"
)

// countPolicySyncs replaces the directory sync for one test; fail makes the
// next sync return an error.
func countPolicySyncs(t *testing.T) (count *int, fail *bool) {
	t.Helper()
	real := syncPolicyDirectory
	count, fail = new(int), new(bool)
	syncPolicyDirectory = func(path string) error {
		*count++
		if *fail {
			*fail = false
			return errors.New("injected directory sync failure")
		}
		return real(path)
	}
	t.Cleanup(func() { syncPolicyDirectory = real })
	return count, fail
}

// The authorization refresh applies its snapshot every second. On a disk
// that stalls, the directory sync each of those made waited for seconds
// although nothing had changed.
func TestRuntimeTransportPolicySyncsOnlyWhatIsNotYetConfirmed(t *testing.T) {
	path, policy := policyFixture(t)
	policy.SourceRevision = 1
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenRuntimeTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	syncs, fail := countPolicySyncs(t)
	same := policy
	same.Revision = 0

	// The first identical snapshot still syncs: this owner has not seen the
	// file it opened made durable, and a previous owner may have left that
	// unknown.
	first, err := owner.Apply(same)
	if err != nil || *syncs != 1 {
		t.Fatalf("first identical apply: syncs=%d err=%v, want one confirming sync", *syncs, err)
	}
	for i := 0; i < 5; i++ {
		got, err := owner.Apply(same)
		if err != nil || !sameTransportPolicy(got, first) {
			t.Fatalf("repeated identical apply = %+v, %v; want %+v", got, err, first)
		}
	}
	if *syncs != 1 {
		t.Fatalf("five unchanged snapshots made %d more directory syncs, want none", *syncs-1)
	}
	if before := owner.current.AllowedKeys; len(before) > 0 {
		got, _ := owner.Apply(same)
		got.AllowedKeys[0] = "caller-owned"
		if owner.current.AllowedKeys[0] == "caller-owned" {
			t.Fatal("an unchanged apply handed out the owner's own key slice")
		}
	}

	// A change is written and synced before it is granted.
	added := same
	added.SourceRevision = 2
	added.AllowedKeys = append(slices.Clone(policy.AllowedKeys), "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	slices.Sort(added.AllowedKeys)
	persisted, err := owner.Apply(added)
	if err != nil || *syncs != 2 || persisted.SourceRevision != 2 {
		t.Fatalf("changed apply: syncs=%d err=%v policy=%+v, want one more sync", *syncs, err, persisted)
	}
	if durable, err := LoadTransportPolicy(path); err != nil || !sameTransportPolicy(durable, persisted) {
		t.Fatalf("file after the change = %+v, %v", durable, err)
	}
	if _, err := owner.Apply(added); err != nil || *syncs != 2 {
		t.Fatalf("the changed snapshot again: syncs=%d err=%v, want no further sync", *syncs, err)
	}

	// A change whose sync fails is not granted, and the next identical
	// snapshot syncs again rather than trusting the file.
	more := added
	more.SourceRevision = 3
	more.AllowedKeys = append(slices.Clone(added.AllowedKeys), "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee")
	slices.Sort(more.AllowedKeys)
	*fail = true
	if _, err := owner.Apply(more); err == nil {
		t.Fatal("a publication whose directory sync failed was reported as applied")
	}
	if snapshot, _ := owner.Snapshot(); snapshot.SourceRevision != 2 {
		t.Fatalf("unconfirmed publication was activated: %+v", snapshot)
	}
	before := *syncs
	if got, err := owner.Apply(more); err != nil || *syncs != before+1 || got.SourceRevision != 3 {
		t.Fatalf("retry after an unknown sync: syncs %d -> %d, err=%v, policy=%+v; want one confirming sync", before, *syncs, err, got)
	}
	if _, err := owner.Apply(more); err != nil || *syncs != before+1 {
		t.Fatalf("after the confirming sync: syncs=%d err=%v, want no further sync", *syncs, err)
	}
}
