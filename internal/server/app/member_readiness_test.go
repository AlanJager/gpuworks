//go:build linux && dqlite

package app

import "testing"

func TestMemberServiceKeepsLivenessBusinessAndSchedulingDistinct(t *testing.T) {
	follower := EvaluateMemberService(true, true, false, true, true)
	if !follower.Live || !follower.BusinessReady || follower.Schedulable {
		t.Fatalf("healthy follower cannot serve business independently of scheduling: %+v", follower)
	}
	leaderWithoutMaterials := EvaluateMemberService(true, false, true, true, true)
	if !leaderWithoutMaterials.Live || !leaderWithoutMaterials.Schedulable || leaderWithoutMaterials.BusinessReady {
		t.Fatalf("missing materials became business ready: %+v", leaderWithoutMaterials)
	}
	leaderWithoutSession := EvaluateMemberService(true, true, true, false, true)
	if leaderWithoutSession.BusinessReady || !leaderWithoutSession.Schedulable {
		t.Fatalf("missing session routing changed scheduling: %+v", leaderWithoutSession)
	}
	offline := EvaluateMemberService(false, true, true, true, true)
	if offline.Live || offline.BusinessReady || offline.Schedulable {
		t.Fatalf("offline member stayed in service: %+v", offline)
	}
	ready := EvaluateMemberService(true, true, true, true, true)
	if !ready.Live || !ready.BusinessReady || !ready.Schedulable {
		t.Fatalf("fully gated member was not ready: %+v", ready)
	}
}
