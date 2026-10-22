package sleep

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

func TestEligibleNamesTheReasonTheUserCanActOn(t *testing.T) {
	supported := deploy.SleepCaps{Supported: true, Levels: []string{"L1", "L2"}}
	for _, tc := range []struct {
		name string
		dep  Deployment
		want string
	}{
		{name: "sleepable vllm", dep: Deployment{Sleepable: true, Caps: supported}, want: ""},
		{name: "not enabled", dep: Deployment{Sleepable: false, Caps: supported}, want: CodeNotEnabled},
		{name: "engine unsupported", dep: Deployment{Sleepable: true, Caps: deploy.SleepCaps{Supported: false}}, want: CodeEngineUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Eligible(tc.dep); got != tc.want {
				t.Fatalf("Eligible = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDeclarableAcceptsNonSleepableDeploymentsOnAnyEngine(t *testing.T) {
	if got := Declarable(Deployment{Sleepable: false}); got != "" {
		t.Fatalf("non-sleepable deployment on an engine without sleep refused: %q", got)
	}
	if got := Declarable(Deployment{Sleepable: true, Caps: deploy.SleepCaps{Supported: true}}); got != "" {
		t.Fatalf("sleepable deployment on a sleeping engine = %q", got)
	}
	if got := Declarable(Deployment{Sleepable: true, Caps: deploy.SleepCaps{Supported: false}}); got != CodeEngineUnsupported {
		t.Fatalf("sleepable on unsupported engine = %q", got)
	}
}

func TestAdmitWakeHandsBackTheSleepersOwnResidue(t *testing.T) {
	const giB = int64(1) << 30
	// The only thing on the card is the sleeper itself: total − 0 − residue +
	// residue leaves the whole card, so a whole-card claim fits.
	card := GPUState{Index: 0, MemoryTotal: 24 * giB, SleepingBytes: ResidentBytes}
	if v := AdmitWake([]GPUState{card}, []Claim{{GPUIndex: 0, Bytes: 24 * giB}}); !v.OK() {
		t.Fatalf("lone sleeper refused its own card: %+v", v.Shortfalls)
	}
	// An awake co-tenant took 20 GiB: the sleeper cannot have its 21.6 GiB back.
	card.AwakeBytes = 20 * giB
	card.Occupants = []string{"newcomer"}
	v := AdmitWake([]GPUState{card}, []Claim{{GPUIndex: 0, Bytes: 216 * giB / 10}})
	if v.OK() || len(v.Shortfalls) != 1 {
		t.Fatalf("wake admitted over an awake co-tenant: %+v", v)
	}
	s := v.Shortfalls[0]
	if s.GPUIndex != 0 || s.NeedBytes != 216*giB/10 || s.FreeBytes != 4*giB || len(s.Occupants) != 1 || s.Occupants[0] != "newcomer" {
		t.Fatalf("shortfall = %+v", s)
	}
	// A second sleeper's residue still counts against the card.
	card.AwakeBytes = 0
	card.SleepingBytes = 2 * ResidentBytes
	card.Occupants = nil
	if v := AdmitWake([]GPUState{card}, []Claim{{GPUIndex: 0, Bytes: 24 * giB}}); v.OK() {
		t.Fatal("wake ignored another sleeper's residue")
	}
	if v := AdmitWake([]GPUState{card}, []Claim{{GPUIndex: 0, Bytes: 24*giB - ResidentBytes}}); !v.OK() {
		t.Fatalf("wake refused despite room for the claim: %+v", v.Shortfalls)
	}
}

func TestAdmitWakeRefusesAClaimOnAnUndescribedGPU(t *testing.T) {
	v := AdmitWake([]GPUState{{Index: 0, MemoryTotal: 1 << 40}}, []Claim{{GPUIndex: 1, Bytes: 1}})
	if v.OK() || v.Shortfalls[0].GPUIndex != 1 || v.Shortfalls[0].FreeBytes != 0 {
		t.Fatalf("undescribed GPU verdict = %+v", v)
	}
}

func TestAdmitWakeSumsClaimsOnTheSameGPU(t *testing.T) {
	const giB = int64(1) << 30
	card := GPUState{Index: 0, MemoryTotal: 32 * giB, AwakeBytes: 17 * giB, SleepingBytes: 2 * ResidentBytes}
	verdict := AdmitWake([]GPUState{card}, []Claim{{GPUIndex: 0, Bytes: 10 * giB}, {GPUIndex: 0, Bytes: 10 * giB}})
	if verdict.OK() || len(verdict.Shortfalls) != 1 {
		t.Fatalf("same-GPU claims were admitted or split: %+v", verdict)
	}
	shortfall := verdict.Shortfalls[0]
	if shortfall.NeedBytes != 20*giB || shortfall.FreeBytes != 15*giB {
		t.Fatalf("same-GPU shortfall = %+v", shortfall)
	}
}
