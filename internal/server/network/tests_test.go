package network

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

func TestStoredTestRuns(t *testing.T) {
	if TestPairCount("tcp", TestPatternMesh, 4) != 12 || TestPairCount("rdma_tool", TestPatternRing, 4) != 4 || TestPairCount("", TestPatternRing, 1) != 0 {
		t.Fatal("pair counts")
	}
	// A card test probes each unordered pair once.
	if TestPairCount(TestCardPing, TestPatternMesh, 4) != 6 || TestPairCount(TestCardMatrix, TestPatternRing, 4) != 4 || TestPairCount(TestCardMatrix, TestPatternRing, 2) != 1 {
		t.Fatal("card pair counts")
	}
	if !CardTest(TestCardPing) || !CardTest(TestCardMatrix) || CardTest("tcp") || CardTest("") {
		t.Fatal("card kinds")
	}
	// 24 nodes of 8 cards: a full matrix is 96 rounds of three seconds.
	nodes := func(n, cards int) [][]netplan.Rail {
		out := make([][]netplan.Rail, n)
		for i := range out {
			for c := 0; c < cards; c++ {
				out[i] = append(out[i], netplan.Rail{Name: fmt.Sprintf("npu%d", c), Addr: fmt.Sprintf("172.20.%d.%d/16", i, c)})
			}
		}
		return out
	}
	plan, err := PlanCardTest(TestCardMatrix, TestPatternMesh, nodes(24, 8))
	if want := (CardTestPlan{Nodes: 24, Pairs: 276, Rounds: 96, Probes: 17664, Seconds: 288, MaxSeconds: 600}); err != nil || plan != want {
		t.Fatalf("plan = %+v %v, want %+v", plan, err, want)
	}
	if plan, err := PlanCardTest(TestCardPing, TestPatternRing, nodes(24, 8)); err != nil || plan.Rounds != 1 || plan.Seconds != 3 || plan.Probes != 192 {
		t.Fatalf("ring plan = %+v %v", plan, err)
	}
	// 60 nodes in a full matrix are 240 rounds: the plan comes back with the
	// refusal, so the caller can say by how much it is over.
	plan, err = PlanCardTest(TestCardMatrix, TestPatternMesh, nodes(60, 8))
	var tooLong *CardTestTooLongError
	if !errors.As(err, &tooLong) || plan.Rounds != 240 || plan.Seconds != 720 || tooLong.Plan != plan || !strings.Contains(err.Error(), "240 rounds") {
		t.Fatalf("over the limit = %+v %v", plan, err)
	}
	// Exactly at the limit passes: 50 nodes are 200 rounds, 600 seconds.
	if plan, err := PlanCardTest(TestCardMatrix, TestPatternMesh, nodes(50, 8)); err != nil || plan.Seconds != 600 {
		t.Fatalf("at the limit = %+v %v", plan, err)
	}
	if plan, err := PlanCardTest(TestCardPing, TestPatternMesh, nil); err != nil || plan != (CardTestPlan{MaxSeconds: 600}) {
		t.Fatalf("no nodes = %+v %v", plan, err)
	}
	// A card run's stored rails read back with what they measured.
	cards := DecodeTestPairs(`[{"source_uuid":"a","target_uuid":"b","reachable":true,"rails":[{"rail":"npu3","target_rail":"npu3","gbps":0,"reachable":true,"rtt_ms":0.25},{"rail":"npu3","target_rail":"npu4","gbps":0,"code":"unreachable","error":"no answer within the time limit"}]}]`)
	if len(cards) != 1 || !cards[0].Rails[0].OK() || cards[0].Rails[0].RTTMS != 0.25 || cards[0].Rails[1].OK() {
		t.Fatalf("card pairs = %+v", cards)
	}
	// Only the same-named pair answered: the diagonal, which is a usable
	// pattern, so the pair passed although a card pair did not answer.
	if CardPairShape(cards[0]) != CardShapeDiagonal || TestPairStatus(TestCardMatrix, cards[0], false) != TestPassed {
		t.Fatalf("shape %s status %s", CardPairShape(cards[0]), TestPairStatus(TestCardMatrix, cards[0], false))
	}
	if got := SummarizeCardTest(cards); got != (CardTestSummary{Shape: CardShapeDiagonal, Probed: 2, Reachable: 1}) {
		t.Fatalf("summary = %+v", got)
	}
	// Outside the card kinds the status is the pair's own.
	if TestPairStatus("tcp", DecodeTestPairs(`[{"reachable":true,"gbps":1}]`)[0], true) != TestPassed {
		t.Fatal("tcp status")
	}
	// A run stored before codes and paired rails still reads.
	pairs := DecodeTestPairs(`[{"source_uuid":"a","target_uuid":"b","reachable":true,"gbps":48.9,"rails":[{"rail":"r0:1","gbps":24.4},{"rail":"r9:1","gbps":0,"error":"exit status 1: ---"}]}]`)
	if len(pairs) != 1 || pairs[0].Status(true) != TestPartial || pairs[0].Rails[1].Rail != "r9:1" {
		t.Fatalf("pairs = %+v", pairs)
	}
	if pairs := DecodeTestPairs(`{broken`); len(pairs) != 0 {
		t.Fatalf("broken results = %+v", pairs)
	}
	if DecodeTestPairs(`[{"reachable":true,"gbps":1}]`)[0].Status(true) != TestPassed || DecodeTestPairs(`[{"code":"offline"}]`)[0].Status(false) != TestFailed {
		t.Fatal("statuses")
	}
}
