package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// realAscendProber is a fixture the host check is allowed to look at: unlike
// FixedProber it does not call itself synthetic.
type realAscendProber struct{ *gpu.FixedProber }

func (realAscendProber) Synthetic() bool { return false }

func ascendDies(n int) []proto.GPUInfo {
	dies := make([]proto.GPUInfo, n)
	for i := range dies {
		dies[i] = proto.GPUInfo{
			Index: i, UUID: fmt.Sprintf("ascend-%d", i), Vendor: "ascend", Name: "Ascend910C",
			PCIeBDF: fmt.Sprintf("0000:%02x:00.0", 0x90+i), Healthy: true, HealthStatus: accelerator.HealthOK,
		}
	}
	return dies
}

// ascendHost is an Ascend node whose per-die check answers as told. try gets
// the die's physical index, as the real check does.
type ascendHost struct {
	mu      sync.Mutex
	prereq  string
	timeout time.Duration
	try     func(ctx context.Context, die string) error
	asked   []string
}

func (h *ascendHost) install(t *testing.T) {
	t.Helper()
	real, ok := gpu.HostRuntimeCheckFor("ascend")
	if !ok {
		t.Fatal("ascend has no host runtime check")
	}
	original := doctorHostRuntimeCheck
	t.Cleanup(func() { doctorHostRuntimeCheck = original })
	doctorHostRuntimeCheck = func(vendor string) (gpu.HostRuntimeCheck, bool) {
		if accelerator.NormalizeVendor(vendor) != accelerator.VendorAscend {
			t.Errorf("the Ascend check asked for the %q runtime", vendor)
		}
		check := real
		check.Prerequisite = func() string { return h.prereq }
		check.Timeout = h.timeout
		check.Try = func(ctx context.Context, die string) error {
			h.mu.Lock()
			h.asked = append(h.asked, die)
			h.mu.Unlock()
			return h.try(ctx, die)
		}
		return check, true
	}
	originalReserve, originalMin := doctorAscendReserve, doctorAscendMinAttempt
	t.Cleanup(func() { doctorAscendReserve, doctorAscendMinAttempt = originalReserve, originalMin })
}

func (h *ascendHost) report(ctx context.Context, dies []proto.GPUInfo) proto.DoctorReport {
	var r proto.DoctorReport
	r.Accelerators = &proto.DoctorAccelerators{Vendor: "ascend"}
	for _, g := range dies {
		r.Accelerators.GPUs = append(r.Accelerators.GPUs, proto.DoctorGPU{
			Index: g.Index, Name: g.Name, PCIeBDF: g.PCIeBDF, Ownership: g.Ownership,
			HealthStatus: g.HealthStatus, HealthCode: g.HealthCode,
		})
	}
	doctorCardRuntimeChecks(ctx, &r, dies)
	return r
}

func failDie(die, detail string) func(context.Context, string) error {
	return func(_ context.Context, got string) error {
		if got == die {
			return errors.New(detail)
		}
		return nil
	}
}

func cardRuntimeFact(r proto.DoctorReport) string {
	v, _ := factValue(r, proto.DoctorFactCardRuntime)
	return v
}

func rowFor(t *testing.T, r proto.DoctorReport, index int) proto.DoctorGPU {
	t.Helper()
	for _, g := range r.Accelerators.GPUs {
		if g.Index == index {
			return g
		}
	}
	t.Fatalf("no row for die %d", index)
	return proto.DoctorGPU{}
}

func findingByCode(r proto.DoctorReport, code string) (proto.DoctorFinding, bool) {
	for _, f := range r.Findings {
		if f.Code == code {
			return f, true
		}
	}
	return proto.DoctorFinding{}, false
}

// The real case: node 192.168.200.29, 14 dies, die 0 fails aclrtSetDevice with
// 507033 and 1-13 pass. Diagnose must show the broken die in the table and as
// a finding while the others stay OK.
func TestDoctorAscendReportsTheDieTheRuntimeCannotUse(t *testing.T) {
	h := &ascendHost{timeout: 45 * time.Second, try: failDie("0", "Ascend ACL runtime smoke failed: aclrtSetDevice=507033")}
	h.install(t)
	r := h.report(context.Background(), ascendDies(14))

	if len(h.asked) != 14 {
		t.Fatalf("the runtime was asked about %v, want every die once", h.asked)
	}
	bad := rowFor(t, r, 0)
	if bad.HealthCode != accelerator.HealthRuntimeUnusable || bad.HealthStatus != accelerator.HealthError {
		t.Fatalf("die 0 row = %+v, want runtime_unusable / Error", bad)
	}
	for i := 1; i < 14; i++ {
		if row := rowFor(t, r, i); row.HealthStatus != accelerator.HealthOK || row.HealthCode != "" {
			t.Fatalf("die %d row = %+v, want it untouched", i, row)
		}
	}
	finding, ok := findingByCode(r, accelerator.HealthRuntimeUnusable)
	if !ok || len(r.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the runtime_unusable one", r.Findings)
	}
	if finding.Level != proto.DoctorLevelError || len(finding.Targets) != 1 {
		t.Fatalf("finding = %+v", finding)
	}
	target := finding.Targets[0]
	if target.Index != 0 || target.PCIeBDF != "0000:90:00.0" {
		t.Fatalf("target = %+v, want die 0", target)
	}
	evidence := map[string]string{}
	for _, f := range target.Evidence {
		evidence[f.Name] = f.Value
	}
	if evidence["Runtime Check"] != "failed" || !strings.Contains(evidence["Detail"], "aclrtSetDevice=507033") || evidence["Checked At"] == "" {
		t.Fatalf("evidence = %+v, want what the agent records for the same failure", target.Evidence)
	}
	// The evidence is a command, not a description of one: the reader has to
	// be able to paste it and watch the same die fail. Measured on
	// 192.168.200.29, where npu-smi calls die 0 healthy and dmesg says
	// nothing about it, so running the check by hand is the only way to see
	// the failure.
	if !strings.Contains(target.Probe, accelerator.AscendACLSmokeScript) {
		t.Errorf("probe does not carry the script the check ran: %q", target.Probe)
	}
	if !strings.Contains(target.Probe, "die='0'") {
		t.Errorf("probe does not name the die it failed on: %q", target.Probe)
	}
	if target.Recheck == "" {
		t.Error("no command for the vendor tool's view of the same die")
	}
	fact := cardRuntimeFact(r)
	for _, want := range []string{"14 dies", "13 passed", "1 failed", "0 not checked", "failed: 0"} {
		if !strings.Contains(fact, want) {
			t.Fatalf("fact = %q, missing %q", fact, want)
		}
	}
}

// The same report, built the way the agent builds it: the table, the finding
// and the text the CLI prints all say it, and the prober's own verdict (OK)
// is what the vendor tool said.
func TestDoctorReportOnAscendShowsTheBrokenDieEverywhere(t *testing.T) {
	withNoTools(t)
	h := &ascendHost{timeout: 45 * time.Second, try: failDie("0", "Ascend ACL runtime smoke failed: aclrtSetDevice=507033")}
	h.install(t)
	p := realAscendProber{&gpu.FixedProber{VendorName: "ascend", GPUs: ascendDies(3)}}

	r := buildDoctorReport(context.Background(), p, nil)

	if g := rowFor(t, r, 0); g.HealthCode != accelerator.HealthRuntimeUnusable {
		t.Fatalf("die 0 = %+v", g)
	}
	if _, ok := findingByCode(r, accelerator.HealthRuntimeUnusable); !ok {
		t.Fatalf("findings = %+v", r.Findings)
	}
	for _, want := range []string{"Error (runtime_unusable)", "[error] runtime_unusable: accelerator 0", "Runtime Check = failed", "aclrtSetDevice=507033", "card runtime", "2 passed, 1 failed, 0 not checked"} {
		if !strings.Contains(r.Report, want) {
			t.Fatalf("report text lacks %q:\n%s", want, r.Report)
		}
	}
	if !strings.Contains(r.Report, "all this node's: 2 OK, 0 Warning, 1 Error") {
		t.Fatalf("the inventory count still says the die is fine:\n%s", r.Report)
	}
}

// A fixture's dies say nothing about the host's CANN, so the real runtime is
// not asked about them.
func TestDoctorReportDoesNotAskTheHostAboutSyntheticAscendDies(t *testing.T) {
	withNoTools(t)
	h := &ascendHost{timeout: time.Second, try: func(context.Context, string) error { t.Error("a synthetic die was checked"); return nil }}
	h.install(t)
	r := buildDoctorReport(context.Background(), &gpu.FixedProber{VendorName: "ascend", GPUs: ascendDies(2)}, nil)
	if _, ok := factValue(r, proto.DoctorFactCardRuntime); ok {
		t.Fatalf("report carries a runtime fact for fixture dies: %+v", r.Facts)
	}
}

// The severity rule is the agent's: a more specific vendor finding stays in
// the table, a weaker one is replaced, and the failure is a finding either
// way.
func TestDoctorAscendKeepsTheMoreSevereVendorVerdictInTheTable(t *testing.T) {
	dies := ascendDies(3)
	dies[0].HealthStatus, dies[0].HealthCode = accelerator.HealthError, accelerator.HealthResetRequired
	dies[1].HealthStatus, dies[1].HealthCode = accelerator.HealthWarning, accelerator.HealthVendorWarning
	h := &ascendHost{timeout: time.Second, try: func(context.Context, string) error { return errors.New("aclrtSetDevice=507033") }}
	h.install(t)
	r := h.report(context.Background(), dies)

	if row := rowFor(t, r, 0); row.HealthCode != accelerator.HealthResetRequired || row.HealthStatus != accelerator.HealthError {
		t.Fatalf("die 0 = %+v, want the vendor's more specific verdict kept", row)
	}
	if row := rowFor(t, r, 1); row.HealthCode != accelerator.HealthRuntimeUnusable || row.HealthStatus != accelerator.HealthError {
		t.Fatalf("die 1 = %+v, want the weaker warning replaced", row)
	}
	if finding, _ := findingByCode(r, accelerator.HealthRuntimeUnusable); len(finding.Targets) != 3 {
		t.Fatalf("targets = %+v, want all three failing dies named", finding.Targets)
	}
}

// A host with the driver but no CANN cannot run the check. That is one fact
// about the host, nothing is asked, and no die is blamed.
func TestDoctorAscendDriverOnlyHostSaysSoOnceAndBlamesNoDie(t *testing.T) {
	h := &ascendHost{prereq: "the Ascend runtime check needs libascendcl.so from a host CANN installation",
		timeout: time.Second, try: func(context.Context, string) error { return errors.New("must not run") }}
	h.install(t)
	r := h.report(context.Background(), ascendDies(8))

	if len(h.asked) != 0 {
		t.Fatalf("the runtime was asked about %v on a host that cannot run the check", h.asked)
	}
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", r.Findings)
	}
	for _, g := range r.Accelerators.GPUs {
		if g.HealthStatus != accelerator.HealthOK || g.HealthCode != "" {
			t.Fatalf("row %+v was changed by a check that did not run", g)
		}
	}
	fact := cardRuntimeFact(r)
	if !strings.Contains(fact, "not run: the Ascend runtime check needs libascendcl.so") || !strings.Contains(fact, "None of the 8 dies was checked") {
		t.Fatalf("fact = %q", fact)
	}
	if strings.Contains(fact, "passed") {
		t.Fatalf("fact = %q implies dies were checked", fact)
	}
}

// A check that never reached the die (CANN would not initialize) is the
// host's answer. The run stops there, keeps what the earlier dies said, and
// blames neither the die it stopped on nor any after it.
func TestDoctorAscendHostUnavailableMidRunStopsAndBlamesNoDie(t *testing.T) {
	h := &ascendHost{timeout: time.Second, try: func(_ context.Context, die string) error {
		switch die {
		case "0":
			return errors.New("Ascend ACL runtime smoke failed: aclrtSetDevice=507033")
		case "3":
			return &gpu.HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime could not be used on this host: aclInit failed"}
		}
		return nil
	}}
	h.install(t)
	r := h.report(context.Background(), ascendDies(8))

	if got := strings.Join(h.asked, ","); got != "0,1,2,3" {
		t.Fatalf("asked %s, want the run to stop at the die where the host gave up", got)
	}
	finding, ok := findingByCode(r, accelerator.HealthRuntimeUnusable)
	if !ok || len(finding.Targets) != 1 || finding.Targets[0].Index != 0 {
		t.Fatalf("findings = %+v, want only die 0, which genuinely failed before the host gave up", r.Findings)
	}
	for i := 1; i < 8; i++ {
		if row := rowFor(t, r, i); row.HealthStatus != accelerator.HealthOK || row.HealthCode != "" {
			t.Fatalf("die %d = %+v, blamed for the host", i, row)
		}
	}
	fact := cardRuntimeFact(r)
	for _, want := range []string{"2 passed", "1 failed", "5 not checked", "stopped: the Ascend ACL runtime could not be used on this host: aclInit failed", "no die is blamed", "3, 4, 5, 6, 7"} {
		if !strings.Contains(fact, want) {
			t.Fatalf("fact = %q, missing %q", fact, want)
		}
	}
}

func TestDoctorAscendHostUnavailableOnTheFirstDieIsNotRunAndBlamesNoDie(t *testing.T) {
	h := &ascendHost{timeout: time.Second, try: func(context.Context, string) error {
		return &gpu.HostRuntimeUnavailableError{Reason: "the Ascend ACL runtime check did not reach the die: signal: killed"}
	}}
	h.install(t)
	r := h.report(context.Background(), ascendDies(4))
	if len(h.asked) != 1 || len(r.Findings) != 0 {
		t.Fatalf("asked %v, findings %+v", h.asked, r.Findings)
	}
	if fact := cardRuntimeFact(r); !strings.HasPrefix(fact, "not run: ") || !strings.Contains(fact, "None of the 4 dies was checked") {
		t.Fatalf("fact = %q", fact)
	}
}

// The remaining time is looked at before each die. Dies that do not fit are
// named as not checked, never dropped and never counted as passed.
func TestDoctorAscendBudgetExhaustionMarksTheRestNotChecked(t *testing.T) {
	deadline := time.Now().Add(600 * time.Millisecond)
	h := &ascendHost{timeout: time.Minute, try: func(_ context.Context, die string) error {
		if die == "1" {
			// Uses up the time: what is left cannot hold another attempt.
			time.Sleep(time.Until(deadline) - 100*time.Millisecond)
		}
		return nil
	}}
	h.install(t)
	doctorAscendReserve, doctorAscendMinAttempt = 0, 200*time.Millisecond
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	r := h.report(ctx, ascendDies(6))

	if got := strings.Join(h.asked, ","); got != "0,1" {
		t.Fatalf("asked %s, want the run to stop once no attempt fits", got)
	}
	fact := cardRuntimeFact(r)
	for _, want := range []string{"2 passed", "0 failed", "4 not checked", "not checked (the diagnosis time budget ran out): 2, 3, 4, 5"} {
		if !strings.Contains(fact, want) {
			t.Fatalf("fact = %q, missing %q", fact, want)
		}
	}
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none for dies nobody asked", r.Findings)
	}
}

// A die cut off by our own deadline had less time than the automatic check
// gives it. It has no verdict, and the dies after it are not checked.
func TestDoctorAscendDieCutOffByTheBudgetIsNotBlamed(t *testing.T) {
	h := &ascendHost{timeout: time.Hour, try: func(ctx context.Context, die string) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	h.install(t)
	doctorAscendReserve, doctorAscendMinAttempt = 0, 10*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	r := h.report(ctx, ascendDies(3))

	if len(r.Findings) != 0 || rowFor(t, r, 0).HealthCode != "" {
		t.Fatalf("findings %+v, row %+v: a die the budget cut off was blamed", r.Findings, rowFor(t, r, 0))
	}
	fact := cardRuntimeFact(r)
	for _, want := range []string{"0 passed", "0 failed", "3 not checked", "while this die was being checked): 0", "1, 2"} {
		if !strings.Contains(fact, want) {
			t.Fatalf("fact = %q, missing %q", fact, want)
		}
	}
}

// The reserve is kept for the rest of the report: a die is not started when
// only the reserve is left.
func TestDoctorAscendLeavesTheReserveForTheRestOfTheReport(t *testing.T) {
	h := &ascendHost{timeout: time.Second, try: func(context.Context, string) error { return nil }}
	h.install(t)
	doctorAscendReserve, doctorAscendMinAttempt = time.Minute, time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r := h.report(ctx, ascendDies(2))

	if len(h.asked) != 0 {
		t.Fatalf("asked %v with less time than the reserve", h.asked)
	}
	if fact := cardRuntimeFact(r); !strings.Contains(fact, "2 not checked") || !strings.Contains(fact, "budget ran out): 0, 1") {
		t.Fatalf("fact = %q", fact)
	}
}

// A die that does not answer within the check's own timeout is failed, as the
// automatic check fails it: a die that hangs is not a healthy one.
func TestDoctorAscendDieThatHangsForItsWholeTimeoutFails(t *testing.T) {
	h := &ascendHost{timeout: 50 * time.Millisecond, try: func(ctx context.Context, die string) error {
		if die == "1" {
			<-ctx.Done()
			return &gpu.HostRuntimeUnavailableError{Reason: "signal: killed"}
		}
		return nil
	}}
	h.install(t)
	doctorAscendReserve, doctorAscendMinAttempt = 0, time.Millisecond

	r := h.report(context.Background(), ascendDies(3))

	finding, ok := findingByCode(r, accelerator.HealthRuntimeUnusable)
	if !ok || len(finding.Targets) != 1 || finding.Targets[0].Index != 1 {
		t.Fatalf("findings = %+v, want die 1", r.Findings)
	}
	if got := cardRuntimeFact(r); !strings.Contains(got, "2 passed") || !strings.Contains(got, "1 failed") {
		t.Fatalf("fact = %q", got)
	}
	if detail := finding.Targets[0].Evidence[2].Value; !strings.Contains(detail, "did not finish within 50ms") {
		t.Fatalf("detail = %q", detail)
	}
}

// Dies the agent leaves alone are left alone, and said to be: serving dies,
// dies whose holder is not known, dies with no device number. Dies that are
// not the node's are not its to report on.
func TestDoctorAscendNamesTheDiesItDoesNotAsk(t *testing.T) {
	dies := ascendDies(6)
	dies[4].Ownership = accelerator.CardPassedThrough
	dies[5].Index = -1
	h := &ascendHost{timeout: time.Second, try: func(context.Context, string) error { return nil }}
	h.install(t)
	held := map[string]bool{accelerator.CardIdent(dies[1].UUID, dies[1].PCIeBDF): true}
	ctx := withDoctorCardsInUse(context.Background(), func() (map[string]bool, bool) { return held, true })

	r := h.report(ctx, dies)

	if got := strings.Join(h.asked, ","); got != "0,2,3" {
		t.Fatalf("asked %s, want only the idle dies this node owns", got)
	}
	fact := cardRuntimeFact(r)
	for _, want := range []string{"the 5 dies this node owns", "3 passed", "2 not checked", "an instance is serving on it", ": 1", "no device number): 0000:95:00.0"} {
		if !strings.Contains(fact, want) {
			t.Fatalf("fact = %q, missing %q", fact, want)
		}
	}
	if strings.Contains(fact, "0000:94") {
		t.Fatalf("fact = %q reports on a die lent to a VM", fact)
	}

	h.asked = nil
	unknown := withDoctorCardsInUse(context.Background(), func() (map[string]bool, bool) { return nil, false })
	r = h.report(unknown, ascendDies(2))
	if len(h.asked) != 0 {
		t.Fatalf("asked %v before the holders of the dies were known", h.asked)
	}
	if got := cardRuntimeFact(r); !strings.Contains(got, "2 not checked") || !strings.Contains(got, "not known yet") {
		t.Fatalf("fact = %q", got)
	}
}

// The doctor RPC the server sends hands the run what the agent knows about
// which dies are serving, so a serving die is left alone.
func TestDoctorRPCLeavesServingAscendDiesAlone(t *testing.T) {
	withNoTools(t)
	h := &ascendHost{timeout: time.Second, try: func(context.Context, string) error { return nil }}
	h.install(t)
	dies := ascendDies(3)
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		realAscendProber{&gpu.FixedProber{VendorName: "ascend", GPUs: dies}}, nil)
	n.heldCards = func() engine.HeldCards {
		return engine.HeldCards{Idents: []string{accelerator.CardIdent(dies[1].UUID, dies[1].PCIeBDF)}}
	}

	out, err := n.OnRPC(context.Background(), proto.RPCNodeDoctor, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := out.(proto.DoctorReport)

	if got := strings.Join(h.asked, ","); got != "0,2" {
		t.Fatalf("asked %s, want the serving die 1 left alone", got)
	}
	if got := cardRuntimeFact(r); !strings.Contains(got, "2 passed") || !strings.Contains(got, "1 not checked") {
		t.Fatalf("fact = %q", got)
	}
}

// A node of another vendor is not asked the Ascend question.
func TestDoctorAscendIgnoresOtherVendors(t *testing.T) {
	withNoTools(t)
	called := false
	original := doctorHostRuntimeCheck
	t.Cleanup(func() { doctorHostRuntimeCheck = original })
	doctorHostRuntimeCheck = func(string) (gpu.HostRuntimeCheck, bool) { called = true; return gpu.HostRuntimeCheck{}, false }
	fakeDiagnosticTools(t)
	stubCUDACheck(t, "PASS fake")
	nvidia := []proto.GPUInfo{{Index: 0, UUID: "GPU-1", Vendor: "nvidia", PCIeBDF: "0000:3f:00.0", Healthy: true, HealthStatus: accelerator.HealthOK}}

	r := buildDoctorReport(context.Background(), realAscendProber{&gpu.FixedProber{VendorName: "nvidia", GPUs: nvidia}}, nil)

	if called {
		t.Fatal("the Ascend check asked for a host runtime on an NVIDIA node")
	}
	if _, ok := factValue(r, proto.DoctorFactCardRuntime); ok {
		t.Fatalf("an NVIDIA report carries the Ascend fact: %+v", r.Facts)
	}
	if _, ok := factValue(r, proto.DoctorFactCUDARuntime); !ok {
		t.Fatalf("the NVIDIA check no longer runs: %+v", r.Facts)
	}
	if rowFor(t, r, 0).HealthStatus != accelerator.HealthOK {
		t.Fatalf("row = %+v", rowFor(t, r, 0))
	}
}

// With no dies of the node's own there is nothing to say.
func TestDoctorAscendWithoutOwnedDiesAddsNothing(t *testing.T) {
	dies := ascendDies(1)
	dies[0].Ownership = accelerator.CardPassedThrough
	h := &ascendHost{timeout: time.Second, try: func(context.Context, string) error { return errors.New("must not run") }}
	h.install(t)
	r := h.report(context.Background(), dies)
	if len(r.Facts) != 0 || len(r.Findings) != 0 || len(h.asked) != 0 {
		t.Fatalf("facts %+v findings %+v asked %v", r.Facts, r.Findings, h.asked)
	}
}

// Hygon cards are asked through HIP, one by one by address, like Ascend dies;
// a card someone outside the platform is using is left alone, as the
// periodic check leaves it.
func TestDoctorAsksEachHygonCardAndSkipsForeignOnes(t *testing.T) {
	real, ok := gpu.HostRuntimeCheckFor("hygon")
	if !ok {
		t.Fatal("hygon has no host runtime check")
	}
	original := doctorHostRuntimeCheck
	t.Cleanup(func() { doctorHostRuntimeCheck = original })
	var asked []string
	doctorHostRuntimeCheck = func(vendor string) (gpu.HostRuntimeCheck, bool) {
		check := real
		check.Prerequisite = func() string { return "" }
		check.Try = func(_ context.Context, bdf string) error {
			asked = append(asked, bdf)
			if bdf == "0000:68:00.0" {
				return errors.New("hygon hip runtime smoke failed: hipMalloc=2")
			}
			return nil
		}
		return check, true
	}
	cards := []proto.GPUInfo{
		{Index: 0, UUID: "hygon-a", Vendor: "hygon", PCIeBDF: "0000:67:00.0", HealthStatus: accelerator.HealthOK},
		{Index: 1, UUID: "hygon-b", Vendor: "hygon", PCIeBDF: "0000:68:00.0", HealthStatus: accelerator.HealthOK},
		{Index: 2, UUID: "hygon-c", Vendor: "hygon", PCIeBDF: "0000:69:00.0", HealthStatus: accelerator.HealthOK},
	}
	var r proto.DoctorReport
	r.Accelerators = &proto.DoctorAccelerators{Vendor: "hygon"}
	for _, g := range cards {
		r.Accelerators.GPUs = append(r.Accelerators.GPUs, proto.DoctorGPU{Index: g.Index, PCIeBDF: g.PCIeBDF, HealthStatus: g.HealthStatus})
	}
	ctx := withDoctorCardsInUse(t.Context(), func() (map[string]bool, bool) { return map[string]bool{}, true })
	foreign := accelerator.CardIdent("hygon-c", "0000:69:00.0")
	ctx = withDoctorForeignCards(ctx, func(ident string) bool { return ident == foreign })
	inFlight := map[string]int{}
	var began []string
	ctx = withDoctorAttempts(ctx, func(ident string) func() {
		inFlight[ident]++
		began = append(began, ident)
		return func() { inFlight[ident]-- }
	})
	doctorCardRuntimeChecks(ctx, &r, cards)
	if len(began) != 2 || inFlight[began[0]] != 0 || inFlight[began[1]] != 0 {
		t.Fatalf("attempts begun %v, in flight after %v: want each checked card marked and released", began, inFlight)
	}

	if strings.Join(asked, ",") != "0000:67:00.0,0000:68:00.0" {
		t.Fatalf("asked %v, want the two cards nobody else uses", asked)
	}
	fact := cardRuntimeFact(r)
	for _, want := range []string{"per-card HIP runtime check of the 3 cards", "1 passed, 1 failed, 1 not checked", "not checked (a process outside the platform is using it): 2"} {
		if !strings.Contains(fact, want) {
			t.Fatalf("fact lacks %q:\n%s", want, fact)
		}
	}
	if row := rowFor(t, r, 1); row.HealthCode != accelerator.HealthRuntimeUnusable {
		t.Fatalf("failed card row = %+v", row)
	}
	if len(r.Findings) != 1 || r.Findings[0].Targets[0].PCIeBDF != "0000:68:00.0" {
		t.Fatalf("findings = %+v", r.Findings)
	}
}
