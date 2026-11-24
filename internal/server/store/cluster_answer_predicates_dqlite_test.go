//go:build linux && dqlite

package store

import "testing"

// The policies answer as the predicates they replace, for every case of the
// table.
func TestAnswerPoliciesAnswerAsThePredicates(t *testing.T) {
	for _, pair := range []struct {
		name      string
		policy    AnswerPolicy
		predicate func(error) bool
	}{
		{"GrantInterrupted", GrantInterrupted, clusterMembershipAttemptInterrupted},
		{"GrantInterrupted (exported)", GrantInterrupted, ClusterGrantInterrupted},
		{"GrantUndecided", GrantUndecided, ClusterGrantUndecided},
	} {
		differences := 0
		for _, c := range append(answerSingles(), answerPairs()...) {
			if got, want := pair.policy.Unanswered(c.err), pair.predicate(c.err); got != want {
				if differences++; differences <= 10 {
					t.Errorf("%s: %s: policy %t, predicate %t", pair.name, c.name, got, want)
				}
			}
		}
		if differences > 10 {
			t.Errorf("%s: %d differences in all", pair.name, differences)
		}
	}
}
