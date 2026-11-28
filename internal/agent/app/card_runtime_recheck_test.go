package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/gpu/cardsmoke"
	"gpuworks.ai/gpuworks/internal/proto"
)

// slowThenAnswer is a runtime check that runs past its deadline the first n
// times it is asked and answers at once after that, as an H20-3e card slow to
// create its first context does.
type slowThenAnswer struct {
	mu   sync.Mutex
	slow int
}

func (s *slowThenAnswer) try(ctx context.Context, _ cardsmoke.Card) error {
	s.mu.Lock()
	slow := s.slow > 0
	s.slow--
	s.mu.Unlock()
	if slow {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func evidenceOf(fields []proto.DoctorField) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		out[f.Name] = f.Value
	}
	return out
}

// A card that timed out and then answered the wider re-check stays usable,
// and its check run says it was re-checked with both budgets, so an operator
// reading it can tell slowness from a fault.
func TestACardThatAnswersTheWiderRecheckStaysUsableAndSaysSo(t *testing.T) {
	n, _, now := checksNode(t)
	try := &slowThenAnswer{slow: 1}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards:       func(context.Context) []cardsmoke.Card { return []cardsmoke.Card{{Ident: cardIdent, Visible: "GPU-3f"}} },
		Try:         try.try,
		Timeout:     20 * time.Millisecond,
		WideTimeout: 300 * time.Millisecond,
		Now:         func() time.Time { return *now },
	})
	n.cardSmoke.RunOnce(context.Background())

	card := reportChecks(n, nil, true)
	if card.HealthCode != "" {
		t.Fatalf("card = %+v, want it left usable", card)
	}
	c := checksOf(card)[accelerator.CheckRuntime]
	if c.Status != proto.CheckPass || len(c.Evidence) != 0 {
		t.Fatalf("runtime check = %+v, want a pass with no evidence in the report", c)
	}
	run := n.checkDetail(cardIdent, accelerator.CheckRuntime).Last
	if run == nil || run.Status != proto.CheckPass {
		t.Fatalf("last run = %+v", run)
	}
	if got := evidenceOf(run.Evidence); got["Timeout"] != "20ms" || got["Recheck Timeout"] != "300ms" || got["Took"] == "" {
		t.Fatalf("last run evidence = %v, want both budgets and how long it took", got)
	}
}

// A card that timed out on the re-check too is unusable, and both its health
// finding and its runtime check carry both budgets beside the detail.
func TestACardThatTimesOutTwiceCarriesBothBudgets(t *testing.T) {
	n, _, now := checksNode(t)
	try := &slowThenAnswer{slow: 2}
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards:       func(context.Context) []cardsmoke.Card { return []cardsmoke.Card{{Ident: cardIdent, Visible: "GPU-3f"}} },
		Try:         try.try,
		Timeout:     20 * time.Millisecond,
		WideTimeout: 60 * time.Millisecond,
		Now:         func() time.Time { return *now },
	})
	n.cardSmoke.RunOnce(context.Background())

	card := reportChecks(n, nil, true)
	if card.HealthCode != accelerator.HealthRuntimeUnusable {
		t.Fatalf("card = %+v, want RUNTIME_UNUSABLE", card)
	}
	for _, want := range []string{"\nTimeout=20ms", "Recheck Timeout=60ms", "nor within 60ms when checked again"} {
		if !strings.Contains(card.HealthMessage, want) {
			t.Fatalf("health message %q does not carry %q", card.HealthMessage, want)
		}
	}
	c := checksOf(card)[accelerator.CheckRuntime]
	got := evidenceOf(c.Evidence)
	if c.Status != proto.CheckFail || got["Timeout"] != "20ms" || got["Recheck Timeout"] != "60ms" || !strings.Contains(got["Detail"], "did not finish within 20ms") {
		t.Fatalf("runtime check = %+v", c)
	}
}

// The re-check RPC gives the runner all its check can take on this node — the
// first attempt, the reap grace and the wider re-check, here the container
// path's 75+35+225 s — and the time to list the cards. The diagnosis's 100 s
// would cut the wider re-check short and report the card as not checked.
func TestTheCardRecheckRPCGivesTheWholeRecheckItsTime(t *testing.T) {
	n, _, now := checksNode(t)
	reportChecks(n, nil, true)
	check := composeCardRuntimeCheck("ppu", gpu.HostRuntimeCheck{}, false, newPPUSmoke(&smokeRuntime{name: "docker"}, ppuImage))
	var left time.Duration
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(ctx context.Context) []cardsmoke.Card {
			if deadline, ok := ctx.Deadline(); ok {
				left = time.Until(deadline)
			}
			return []cardsmoke.Card{{Ident: cardIdent, Visible: "GPU-3f"}}
		},
		Try:       func(context.Context, cardsmoke.Card) error { return nil },
		Timeout:   check.Timeout,
		ReapGrace: check.ReapGrace,
		Now:       func() time.Time { return *now },
	})
	params, _ := json.Marshal(proto.CardRecheckRequest{Ident: cardIdent})

	if _, err := n.OnRPC(context.Background(), proto.RPCNodeCardRecheck, params); err != nil {
		t.Fatal(err)
	}

	if want := n.cardSmoke.CheckCardBudget(); want < 75*time.Second+35*time.Second+225*time.Second || left < want+cardRecheckListing-time.Second {
		t.Fatalf("the RPC left %s for a check that can take %s and the listing", left, want)
	}
	if left > proto.CardRecheckTimeout {
		t.Fatalf("the RPC left %s, more than the %s the server waits for", left, proto.CardRecheckTimeout)
	}
}

// Every vendor and path fits the bound the server waits for, so none is cut
// to less than its check and the listing.
func TestEveryCardCheckPathFitsTheRecheckBound(t *testing.T) {
	host := func(vendor string) gpu.HostRuntimeCheck {
		check, ok := gpu.HostRuntimeCheckFor(vendor)
		if !ok {
			t.Fatalf("%s has no host runtime check", vendor)
		}
		return check
	}
	paths := map[string]gpu.HostRuntimeCheck{
		"nvidia":                  host("nvidia"),
		"hygon":                   host("hygon"),
		"ascend on kubernetes":    host("ascend"),
		"ascend beside a runtime": composeCardRuntimeCheck("ascend", host("ascend"), true, newPPUSmoke(&smokeRuntime{name: "docker"}, ppuImage)),
		"ppu in a container":      composeCardRuntimeCheck("ppu", gpu.HostRuntimeCheck{}, false, newPPUSmoke(&smokeRuntime{name: "docker"}, ppuImage)),
	}
	for name, check := range paths {
		r := cardsmoke.New(cardsmoke.Config{Timeout: check.Timeout, ReapGrace: check.ReapGrace})
		if got, want := cardRecheckBudget(r), r.CheckCardBudget()+cardRecheckListing; got != want {
			t.Errorf("%s: re-check budget %s, want its check and the listing, %s, within %s", name, got, want, proto.CardRecheckTimeout)
		}
	}
	if got := cardRecheckBudget(nil); got != cardRecheckListing {
		t.Fatalf("budget with no runner = %s", got)
	}
}

// A check in a container returns only once the container is stopped and
// removed, up to containerSmokeCleanupBudget after its deadline. Its reap
// grace covers that, or a card that timed out there would never earn the
// wider re-check; the host's own check keeps the runner's default.
func TestAContainerCheckWaitsOutItsCleanupBeforeGivingUpOnTheRecheck(t *testing.T) {
	for name, check := range map[string]gpu.HostRuntimeCheck{
		"ppu":    composeCardRuntimeCheck("ppu", gpu.HostRuntimeCheck{}, false, newPPUSmoke(&smokeRuntime{name: "docker"}, ppuImage)),
		"ascend": composeCardRuntimeCheck("ascend", gpu.HostRuntimeCheck{CLI: "npu-smi", Timeout: 45 * time.Second}, true, newPPUSmoke(&smokeRuntime{name: "docker"}, ppuImage)),
	} {
		if check.ReapGrace <= containerSmokeCleanupBudget {
			t.Errorf("%s: reap grace %s does not cover the container's cleanup of up to %s", name, check.ReapGrace, containerSmokeCleanupBudget)
		}
	}
	if check, _ := gpu.HostRuntimeCheckFor("nvidia"); check.ReapGrace != 0 {
		t.Fatalf("the host check's reap grace = %s, want the runner's default", check.ReapGrace)
	}
}

// The periodic check takes its reap grace from the node's check, so a PPU
// card checked in a container is waited for while its container is removed.
func TestThePeriodicCheckWaitsOutAContainerChecksCleanup(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), &imagePullTestDriver{}, t.TempDir(), "",
		realPPUProber{&gpu.FixedProber{VendorName: "ppu"}}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.startCardSmoke(ctx)
	if n.cardSmoke == nil {
		t.Fatal("no runtime check on a PPU node with a container runtime")
	}
	if got, least := n.cardSmoke.CheckCardBudget(), 75*time.Second+containerSmokeCleanupBudget+225*time.Second; got <= least {
		t.Fatalf("check budget %s, want the first attempt, a reap grace beyond the container's cleanup and the re-check (more than %s)", got, least)
	}
}

type realPPUProber struct{ *gpu.FixedProber }

func (realPPUProber) Synthetic() bool { return false }

// A die that does not answer within the check's own timeout is reported as
// timed out by the diagnosis, with no verdict, and not recorded for the
// periodic check: that one judges a timeout only after one more try with a
// wider budget, which a diagnosis does not make, so a die merely slow to
// create its first context must not be shown as failed or taken out of
// scheduling by a click on Diagnose. The periodic verdict stays as it was — a
// pass, or none at all — with no backoff.
func TestDoctorTimeoutLeavesThePeriodicVerdictAsItWas(t *testing.T) {
	h := &ascendHost{timeout: 50 * time.Millisecond, try: func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	h.install(t)
	doctorAscendReserve, doctorAscendMinAttempt = 0, 10*time.Millisecond
	dies := ascendDies(2)
	passed := accelerator.CardIdent(dies[0].UUID, dies[0].PCIeBDF)
	never := accelerator.CardIdent(dies[1].UUID, dies[1].PCIeBDF)
	runner := cardsmoke.New(cardsmoke.Config{Cards: func(context.Context) []cardsmoke.Card { return nil }})
	runner.Record(passed, nil)
	before := runner.Outcomes()[passed]

	r := h.report(withDoctorOutcomes(context.Background(), runner.Record), dies)

	if got := runner.Outcomes(); len(got) != 1 || got[passed] != before {
		t.Fatalf("periodic outcomes after a doctor timeout = %+v, want only the earlier pass %+v", got, before)
	}
	if _, ok := runner.Outcomes()[never]; ok {
		t.Fatal("a die never checked by the periodic check got a verdict from a doctor timeout")
	}
	if fact := cardRuntimeFact(r); !strings.Contains(fact, "0 passed, 0 failed, 2 not checked") {
		t.Fatalf("fact = %q, want both timeouts reported without a verdict", fact)
	}
	finding, ok := findingByCode(r, proto.DoctorSkipRuntimeCheckTimedOut)
	if !ok || finding.Status != proto.CheckSkip || len(finding.Targets) != 2 {
		t.Fatalf("findings = %+v, want both dies in the timed-out finding", r.Findings)
	}
	reason := evidenceOf(finding.Targets[0].Evidence)["reason"]
	if !strings.Contains(reason, "did not finish within 50ms") || !strings.Contains(reason, "follows the periodic check") {
		t.Fatalf("reason = %q, want the timeout and that scheduling follows the periodic check", reason)
	}
}

// "Recheck" on a card whose periodic check timed out and is waiting for the
// killed attempt or checking it again says that a check is in progress, not
// that the host cannot be checked: the periodic re-check answers for the card,
// and its answer is what the card shows once it is in.
func TestTheCardRecheckRPCDuringTheCardsRecheckSaysItIsInProgress(t *testing.T) {
	n, _, now := checksNode(t)
	reportChecks(n, nil, true)
	release, expired := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	asked := 0
	n.cardSmoke = cardsmoke.New(cardsmoke.Config{
		Cards: func(context.Context) []cardsmoke.Card { return []cardsmoke.Card{{Ident: cardIdent, Visible: "GPU-3f"}} },
		Try: func(ctx context.Context, _ cardsmoke.Card) error {
			mu.Lock()
			asked++
			first := asked == 1
			mu.Unlock()
			if first {
				// Past its deadline, back only once released.
				<-ctx.Done()
				close(expired)
				<-release
				return context.DeadlineExceeded
			}
			return nil
		},
		Timeout:     20 * time.Millisecond,
		WideTimeout: 300 * time.Millisecond,
		ReapGrace:   5 * time.Second,
		Now:         func() time.Time { return *now },
	})
	round := make(chan struct{})
	go func() {
		defer close(round)
		n.cardSmoke.RunOnce(context.Background())
	}()
	<-expired
	// The runner marks the card at the same deadline; give it a moment.
	time.Sleep(20 * time.Millisecond)
	params, _ := json.Marshal(proto.CardRecheckRequest{Ident: cardIdent})

	raw, err := n.OnRPC(context.Background(), proto.RPCNodeCardRecheck, params)
	if err != nil {
		t.Fatal(err)
	}
	detail := raw.(proto.CardCheckDetail)
	if detail.Last == nil || detail.Last.Status != proto.CheckSkip || detail.Last.Code != proto.CardCheckSkipInProgress {
		t.Fatalf("recheck during the card's own re-check = %+v, want skip %s", detail.Last, proto.CardCheckSkipInProgress)
	}

	close(release)
	<-round
	if o := n.cardSmoke.Outcomes()[cardIdent]; !o.OK || !o.Recheck.Made() {
		t.Fatalf("outcome = %+v, want the periodic re-check's pass", o)
	}
}
