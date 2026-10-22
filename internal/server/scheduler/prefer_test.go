package scheduler

import "testing"

// preferNode is a node of total identical 80 GiB GPUs, the first busy of which
// are fully allocated.
func preferNode(total, busy int) NodeSnapshot {
	gpus := make([]GPU, total)
	for index := range gpus {
		gpus[index] = gpu(index, 80, index >= busy, 0)
	}
	return NodeSnapshot{UUID: "prefer", DtypeSupported: true, DiskAvailBytes: 1 << 50, GPUs: gpus}
}

// fourOrEight accepts only 4 or 8 GPUs and states the given preference for
// each accepted count.
func fourOrEight(prefer func(count int) int64) func([]GPU) CandidateEvaluation {
	return func(candidate []GPU) CandidateEvaluation {
		if len(candidate) != 4 && len(candidate) != 8 {
			return CandidateEvaluation{RejectionReason: "GPU count must be 4 or 8"}
		}
		return CandidateEvaluation{Preference: prefer(len(candidate))}
	}
}

func preferRequest(evaluate func([]GPU) CandidateEvaluation) Request {
	return Request{WeightBytes: giB, HeadCount: 32, GPUMemUtil: 0.9, EvaluateCandidate: evaluate}
}

func TestSchedulePreferenceChoosesTheHighestPreferredGPUCount(t *testing.T) {
	result := Schedule(preferRequest(fourOrEight(func(count int) int64 { return int64(count) })), preferNode(8, 0))
	if !result.OK || result.Plan.TP != 8 || len(result.Plan.GPUIndexes) != 8 {
		t.Fatalf("prefer=count on a free 8-GPU node: %+v", result)
	}
	if result.Plan.Preference != 8 {
		t.Fatalf("plan preference = %d, want 8", result.Plan.Preference)
	}
	var stated *Reason
	for index := range result.Plan.Reasons {
		if result.Plan.Reasons[index].Code == ReasonPlacementPreference {
			stated = &result.Plan.Reasons[index]
		}
	}
	if stated == nil || stated.Score != 0 || stated.Params["count"] != 8 || stated.Params["preference"] != int64(8) {
		t.Fatalf("explain lacks the preference reason: %+v", result.Plan.Reasons)
	}
}

func TestSchedulePreferenceFallsBackToWhatFits(t *testing.T) {
	// Only four GPUs are free, so the preferred eight is not a candidate.
	result := Schedule(preferRequest(fourOrEight(func(count int) int64 { return int64(count) })), preferNode(8, 4))
	if !result.OK || result.Plan.TP != 4 {
		t.Fatalf("prefer=count with four free GPUs: %+v", result)
	}
}

func TestSchedulePreferenceDoesNotOverrideAUserPinnedCount(t *testing.T) {
	request := preferRequest(fourOrEight(func(count int) int64 { return int64(count) }))
	request.AllowedTPs = []int{4}
	result := Schedule(request, preferNode(8, 0))
	if !result.OK || result.Plan.TP != 4 {
		t.Fatalf("a pinned parallelism must stay 4 whatever the resolver prefers: %+v", result)
	}
}

func TestSchedulePreferenceCanFavourFewerGPUs(t *testing.T) {
	result := Schedule(preferRequest(fourOrEight(func(count int) int64 { return int64(-count) })), preferNode(8, 0))
	if !result.OK || result.Plan.TP != 4 || result.Plan.Preference != -4 {
		t.Fatalf("prefer=-count: %+v", result)
	}
}

func TestScheduleEqualPreferenceKeepsTheOrdinaryScoring(t *testing.T) {
	result := Schedule(preferRequest(fourOrEight(func(int) int64 { return 7 })), preferNode(8, 0))
	if !result.OK || result.Plan.TP != 4 {
		t.Fatalf("equal preference must keep the packing winner (fewest GPUs): %+v", result)
	}
	// The same shape with no preference at all is the unchanged behaviour.
	result = Schedule(preferRequest(fourOrEight(func(int) int64 { return 0 })), preferNode(8, 0))
	if !result.OK || result.Plan.TP != 4 || result.Plan.Preference != 0 {
		t.Fatalf("zero preference: %+v", result)
	}
	for _, reason := range result.Plan.Reasons {
		if reason.Code == ReasonPlacementPreference {
			t.Fatalf("a zero preference leaked into the explanation: %+v", result.Plan.Reasons)
		}
	}
}

func TestSchedulePreferenceDoesNotLeakIntoTheScore(t *testing.T) {
	preferred := Schedule(preferRequest(fourOrEight(func(count int) int64 { return int64(count) })), preferNode(8, 0))
	plain := Schedule(preferRequest(fourOrEight(func(int) int64 { return 0 })), preferNode(8, 0))
	if preferred.Plan.TP != 8 || plain.Plan.TP != 4 {
		t.Fatalf("setup: preferred=%d plain=%d", preferred.Plan.TP, plain.Plan.TP)
	}
	// Eight GPUs pay the packing penalty (-50 per extra GPU); a preference
	// summed into the score would show up as a higher total.
	if preferred.Plan.Score > plain.Plan.Score {
		t.Fatalf("preferred score %d exceeds the unpreferred %d: preference leaked into Score", preferred.Plan.Score, plain.Plan.Score)
	}
}
