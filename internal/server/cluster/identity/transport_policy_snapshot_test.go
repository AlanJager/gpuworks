package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/filelock"
)

func TestTransportPolicySourceRevisionStrictJSON(t *testing.T) {
	path, policy := policyFixture(t)
	base, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, value string
		valid       bool
	}{{"legacy", "", true}, {"one", "1", true}, {"maximum", "18446744073709551615", true}, {"zero", "0", false}, {"null", "null", false}, {"negative", "-1", false}, {"fraction", "1.5", false}, {"float", "1.0", false}, {"exponent", "1e2", false}, {"string", `"1"`, false}, {"overflow", "18446744073709551616", false}} {
		t.Run(test.name, func(t *testing.T) {
			data := append([]byte(nil), base...)
			if test.value != "" {
				data = append(data[:len(data)-1], []byte(`,"source_revision":`+test.value+`}`)...)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := LoadTransportPolicy(path)
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
				if test.value == "" && got.SourceRevision != 0 {
					t.Fatal("legacy was source-bound")
				}
			} else if err == nil {
				t.Fatalf("invalid value accepted %#v", got)
			}
		})
	}
	for _, suffix := range []string{`,"source_revision":1,"source_revision":1}`, `,"source_revision":1,"future":1}`} {
		data := append(append([]byte(nil), base[:len(base)-1]...), []byte(suffix)...)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadTransportPolicy(path); err == nil {
			t.Fatal("duplicate/unknown fields accepted")
		}
	}
	// Optional source_revision must not substitute for a required legacy field.
	var object map[string]any
	if err := json.Unmarshal(base, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, "revoked_keys")
	object["source_revision"] = 1
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadTransportPolicy(path); err == nil {
		t.Fatal("incomplete legacy fields accepted")
	}
}

func TestApplyTransportPolicySnapshotIndependentRevisions(t *testing.T) {
	path, policy := policyFixture(t)
	snapshot := policy
	snapshot.Revision = 0
	snapshot.SourceRevision = 10
	first, err := ApplyTransportPolicySnapshot(path, snapshot)
	if err != nil || first.Revision != 1 || first.SourceRevision != 10 {
		t.Fatalf("initial %#v %v", first, err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyTransportPolicySnapshot(path, snapshot)
	if err != nil || !reflect.DeepEqual(repeated, first) {
		t.Fatalf("retry %#v %v", repeated, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatalf("retry rewrote policy: %v", err)
	}
	snapshot.SourceRevision = 1000
	snapshot.AllowedKeys = []string{policy.AllowedKeys[1]}
	snapshot.RevokedKeys = []string{policy.AllowedKeys[0]}
	next, err := ApplyTransportPolicySnapshot(path, snapshot)
	if err != nil || next.Revision != 2 || next.SourceRevision != 1000 {
		t.Fatalf("source jump %#v %v", next, err)
	}
	loaded, err := LoadTransportPolicy(path)
	if err != nil || !reflect.DeepEqual(loaded, next) {
		t.Fatalf("restart %#v %v", loaded, err)
	}
	release, err := LockTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.SourceRevision++
	if result, err := ApplyTransportPolicySnapshot(path, snapshot); !errors.Is(err, filelock.ErrBusy) || !reflect.DeepEqual(result, TransportPolicy{}) {
		_ = release()
		t.Fatalf("runtime policy lock bypassed %#v %v", result, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	final, err := ApplyTransportPolicySnapshot(path, snapshot)
	if err != nil || final.Revision != 3 || final.SourceRevision != 1001 {
		t.Fatalf("released policy lock %#v %v", final, err)
	}
}

func TestTransportPolicySnapshotRejectsStaleAndResurrection(t *testing.T) {
	path, policy := policyFixture(t)
	if err := PublishTransportPolicy(path, 0, policy); err != nil {
		t.Fatal(err)
	}
	snapshot := policy
	snapshot.Revision = 0
	snapshot.SourceRevision = 20
	snapshot.AllowedKeys = []string{policy.AllowedKeys[1]}
	snapshot.RevokedKeys = []string{policy.AllowedKeys[0]}
	current, err := ApplyTransportPolicySnapshot(path, snapshot)
	if err != nil || current.Revision != 2 {
		t.Fatalf("legacy upgrade %#v %v", current, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"older", "same different", "resurrect", "drop tombstone", "drop live", "authority", "local revision", "zero source"} {
		t.Run(name, func(t *testing.T) {
			bad := snapshot
			switch name {
			case "older":
				bad.SourceRevision--
			case "same different":
				bad.AllowedKeys = append(append([]string(nil), bad.AllowedKeys...), strings.Repeat("c", 64))
			case "resurrect":
				bad.SourceRevision++
				bad.AllowedKeys = policy.AllowedKeys
				bad.RevokedKeys = nil
			case "drop tombstone":
				bad.SourceRevision++
				bad.RevokedKeys = nil
			case "drop live":
				bad.SourceRevision++
				bad.AllowedKeys = []string{strings.Repeat("c", 64)}
			case "authority":
				bad.SourceRevision++
				bad.CAFingerprint = strings.Repeat("d", 64)
			case "local revision":
				bad.Revision = 1
			case "zero source":
				bad.SourceRevision = 0
			}
			if result, err := ApplyTransportPolicySnapshot(path, bad); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
				t.Fatalf("bad snapshot %#v %v", result, err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("error modified durable policy %v", err)
			}
		})
	}
	for _, source := range []uint64{0, 19} {
		bad := current
		bad.Revision++
		bad.SourceRevision = source
		if err := PublishTransportPolicy(path, current.Revision, bad); err == nil {
			t.Fatal("raw publication downgraded source")
		}
	}
	bad := current
	bad.Revision++
	bad.AllowedKeys = append(append([]string(nil), bad.AllowedKeys...), strings.Repeat("c", 64))
	if err := PublishTransportPolicy(path, current.Revision, bad); err == nil {
		t.Fatal("raw publication changed same-version source")
	}
	// Exact source state may be locally republished; source and local counters
	// intentionally serve different purposes.
	same := current
	same.Revision++
	if err := PublishTransportPolicy(path, current.Revision, same); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyTransportPolicySnapshot(path, snapshot)
	if err != nil || result.Revision != same.Revision {
		t.Fatalf("source retry after local publication %#v %v", result, err)
	}
}

func TestApplyTransportPolicySnapshotRejectsDamagedState(t *testing.T) {
	path, policy := policyFixture(t)
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := policy
	snapshot.Revision = 0
	snapshot.SourceRevision = 1
	if result, err := ApplyTransportPolicySnapshot(path, snapshot); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
		t.Fatalf("damaged file repaired %#v %v", result, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "broken" {
		t.Fatalf("damaged source modified %v", err)
	}
}

func TestApplyTransportPolicySnapshotConcurrentSources(t *testing.T) {
	for _, sameVersion := range []bool{false, true} {
		t.Run(map[bool]string{false: "different_versions", true: "conflicting_same_version"}[sameVersion], func(t *testing.T) {
			path, base := policyFixture(t)
			base.Revision = 0
			base.SourceRevision = 1
			if _, err := ApplyTransportPolicySnapshot(path, base); err != nil {
				t.Fatal(err)
			}
			candidates := []TransportPolicy{base, base}
			candidates[0].SourceRevision = 10
			candidates[1].SourceRevision = 20
			if sameVersion {
				candidates[1].SourceRevision = 10
				candidates[0].AllowedKeys = append(append([]string(nil), base.AllowedKeys...), strings.Repeat("c", 64))
				candidates[1].AllowedKeys = append(append([]string(nil), base.AllowedKeys...), strings.Repeat("d", 64))
			}
			type attempt struct {
				policy TransportPolicy
				err    error
			}
			results := make([]attempt, 2)
			start := make(chan struct{})
			done := make(chan int, 2)
			for i := range candidates {
				go func() {
					<-start
					results[i].policy, results[i].err = ApplyTransportPolicySnapshot(path, candidates[i])
					done <- i
				}()
			}
			close(start)
			<-done
			<-done
			// Observe durable state only after both writers finish: an earlier successful
			// return can legitimately have been superseded by the other publication.
			persisted, err := LoadTransportPolicy(path)
			if err != nil {
				t.Fatal(err)
			}
			successes := 0
			winner := -1
			for i, result := range results {
				if result.err == nil {
					successes++
					winner = i
					if result.policy.SourceRevision != candidates[i].SourceRevision || !reflect.DeepEqual(result.policy.AllowedKeys, candidates[i].AllowedKeys) || result.policy.Revision < 2 || result.policy.Revision > persisted.Revision {
						t.Fatalf("invalid successful result %#v, durable %#v", result.policy, persisted)
					}
				} else if !reflect.DeepEqual(result.policy, TransportPolicy{}) {
					t.Fatalf("error leaked policy %#v %v", result.policy, result.err)
				}
			}
			if successes == 0 {
				t.Fatalf("neither concurrent caller published: %v / %v", results[0].err, results[1].err)
			}
			if sameVersion {
				if successes != 1 {
					t.Fatalf("same source version published conflicting states: %#v", results)
				}
				if !reflect.DeepEqual(persisted, results[winner].policy) {
					t.Fatalf("sole winner differs from durable policy: %#v %#v", results[winner].policy, persisted)
				}
				loser := 1 - winner
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if result, err := ApplyTransportPolicySnapshot(path, candidates[loser]); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
					t.Fatalf("explicit conflict retry accepted %#v %v", result, err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("conflict retry changed winner: %v", err)
				}
				return
			}
			// If the newer contender encountered a busy lease, explicitly submit it
			// again. It must either advance the source or confirm its prior publication.
			latest, err := ApplyTransportPolicySnapshot(path, candidates[1])
			if err != nil || latest.SourceRevision != 20 {
				t.Fatalf("newest retry %#v %v", latest, err)
			}
			if stale, err := ApplyTransportPolicySnapshot(path, candidates[0]); err == nil || !reflect.DeepEqual(stale, TransportPolicy{}) {
				t.Fatalf("stale retry accepted %#v %v", stale, err)
			}
			final, err := LoadTransportPolicy(path)
			if err != nil || !reflect.DeepEqual(final, latest) {
				t.Fatalf("quiescent durable result %#v, returned %#v, %v", final, latest, err)
			}
		})
	}
}

func TestApplyTransportPolicySnapshotExhaustedLocalRevision(t *testing.T) {
	path, current := policyFixture(t)
	current.Revision = ^uint64(0)
	current.SourceRevision = 42
	// A terminal local counter cannot be reached with a practical number of
	// publications. Install a valid persisted fixture and exercise real loading.
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := current
	snapshot.Revision = 0
	repeated, err := ApplyTransportPolicySnapshot(path, snapshot)
	if err != nil || !reflect.DeepEqual(repeated, current) {
		t.Fatalf("exhausted exact retry %#v %v", repeated, err)
	}
	snapshot.SourceRevision++
	if result, err := ApplyTransportPolicySnapshot(path, snapshot); err == nil || !reflect.DeepEqual(result, TransportPolicy{}) {
		t.Fatalf("exhausted counter advanced %#v %v", result, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("exhaustion changed durable state %v", err)
	}
	loaded, err := LoadTransportPolicy(path)
	if err != nil || !reflect.DeepEqual(loaded, current) {
		t.Fatalf("exhausted restart %#v %v", loaded, err)
	}
}
