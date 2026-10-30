package planning

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/sleep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Every caller's level - an operator's 1 or 2, the automation's auto (0), a
// holder's policy preference - reads as one of the three preferences.
func TestSleepPreferenceReadsEveryLevelVocabulary(t *testing.T) {
	for level, want := range map[int]string{
		proto.SleepLevelL1: servicelevel.LevelL1,
		proto.SleepLevelL2: servicelevel.LevelL2,
		0:                  servicelevel.LevelAuto,
		7:                  servicelevel.LevelAuto,
	} {
		if got := SleepPreference(level); got != want {
			t.Fatalf("SleepPreference(%d) = %q, want %q", level, got, want)
		}
	}
}

// The resolution matrix: auto falls back to L2 where the host cannot hold the
// weights, an explicit L1 is refused there, L2 is never refused, and a host
// whose memory is unknown refuses nothing.
func TestResolveSleepLevelMatrix(t *testing.T) {
	st, _, dep := residueFixture(t, 40<<30, "bfloat16")
	insts, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	roomy := func(string) (int64, int64, bool) { return 100 << 30, 256 << 30, true }
	tight := func(string) (int64, int64, bool) { return 40 << 30, 64 << 30, true }
	unknown := func(string) (int64, int64, bool) { return 0, 0, false }
	for _, tc := range []struct {
		name    string
		pref    string
		host    HostMemory
		want    int
		refused bool
	}{
		{"auto, host holds the weights", servicelevel.LevelAuto, roomy, proto.SleepLevelL1, false},
		{"auto, host cannot", servicelevel.LevelAuto, tight, proto.SleepLevelL2, false},
		{"auto, host unknown", servicelevel.LevelAuto, unknown, proto.SleepLevelL1, false},
		{"auto, no host reader", servicelevel.LevelAuto, nil, proto.SleepLevelL1, false},
		{"l1, host holds the weights", servicelevel.LevelL1, roomy, proto.SleepLevelL1, false},
		{"l1, host cannot", servicelevel.LevelL1, tight, 0, true},
		{"l1, host unknown", servicelevel.LevelL1, unknown, proto.SleepLevelL1, false},
		{"l2, host holds the weights", servicelevel.LevelL2, roomy, proto.SleepLevelL2, false},
		{"l2, host cannot", servicelevel.LevelL2, tight, proto.SleepLevelL2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			level, refusal := ResolveSleepLevel(st, dep, insts, tc.pref, tc.host)
			if level != tc.want || (refusal != nil) != tc.refused {
				t.Fatalf("level %d refusal %+v, want level %d refused %v", level, refusal, tc.want, tc.refused)
			}
			if refusal != nil && refusal.Code != sleep.CodeHostMemoryInsufficient {
				t.Fatalf("refusal code %q", refusal.Code)
			}
		})
	}
}

// OrderSleep writes the order at the resolved level, held or not, on the
// whole deployment or the named instances; a refusal writes nothing.
func TestOrderSleepWritesTheResolvedLevelOrNothing(t *testing.T) {
	tight := func(string) (int64, int64, bool) { return 40 << 30, 64 << 30, true }
	intent := func(t *testing.T, st *store.Store) (int, bool) {
		t.Helper()
		in, err := st.InstanceByUUID("residue-1")
		if err != nil {
			t.Fatal(err)
		}
		dep, err := st.GetDeployment(in.DeploymentID)
		if err != nil {
			t.Fatal(err)
		}
		return InstanceSleepIntent(in), dep.SleepHeld
	}

	t.Run("an explicit L1 the host cannot hold is refused and writes nothing", func(t *testing.T) {
		st, _, dep := residueFixture(t, 40<<30, "bfloat16")
		level, refusal, err := OrderSleep(st, dep, SleepOrder{Pref: servicelevel.LevelL1, Mode: proto.SleepModeWait, Held: true}, tight)
		if err != nil || refusal == nil || refusal.Code != sleep.CodeHostMemoryInsufficient || level != 0 {
			t.Fatalf("level %d refusal %+v err %v", level, refusal, err)
		}
		if got, _ := intent(t, st); got != 0 {
			t.Fatalf("a refused order wrote intent %d", got)
		}
		var product *messages.ProductError
		if !errors.As(refusal.Err(), &product) || product.Code() != sleep.CodeHostMemoryInsufficient {
			t.Fatalf("refusal as an error = %v", refusal.Err())
		}
	})

	t.Run("auto falls back to L2 for the whole deployment, held", func(t *testing.T) {
		st, _, dep := residueFixture(t, 40<<30, "bfloat16")
		level, refusal, err := OrderSleep(st, dep, SleepOrder{Pref: servicelevel.LevelAuto, Mode: proto.SleepModeWait, Held: true}, tight)
		if err != nil || refusal != nil || level != proto.SleepLevelL2 {
			t.Fatalf("level %d refusal %+v err %v", level, refusal, err)
		}
		if got, held := intent(t, st); got != proto.SleepLevelL2 || !held {
			t.Fatalf("intent %d held %v, want L2 held", got, held)
		}
	})

	t.Run("a yield names its instances and is not held", func(t *testing.T) {
		st, _, dep := residueFixture(t, 40<<30, "bfloat16")
		level, refusal, err := OrderSleep(st, dep, SleepOrder{Pref: servicelevel.LevelAuto, Mode: proto.SleepModeAbort, Instances: []string{"residue-1"}}, nil)
		if err != nil || refusal != nil || level != proto.SleepLevelL1 {
			t.Fatalf("level %d refusal %+v err %v", level, refusal, err)
		}
		if got, held := intent(t, st); got != proto.SleepLevelL1 || held {
			t.Fatalf("intent %d held %v, want L1 not held", got, held)
		}
	})

	t.Run("an instance no longer the deployment's is the store's not found", func(t *testing.T) {
		st, _, dep := residueFixture(t, 40<<30, "bfloat16")
		_, refusal, err := OrderSleep(st, dep, SleepOrder{Pref: servicelevel.LevelL2, Mode: proto.SleepModeWait, Instances: []string{"gone"}}, nil)
		if refusal != nil || !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("refusal %+v err %v, want store.ErrNotFound", refusal, err)
		}
	})

	t.Run("a deployment that cannot sleep is refused at any level", func(t *testing.T) {
		st, _, dep := residueFixture(t, 40<<30, "bfloat16")
		dep.Sleepable = false
		for _, pref := range servicelevel.SleepLevels {
			_, refusal, err := OrderSleep(st, dep, SleepOrder{Pref: pref, Mode: proto.SleepModeWait}, nil)
			if err != nil || refusal == nil || refusal.Code != sleep.CodeNotEnabled {
				t.Fatalf("%s: refusal %+v err %v", pref, refusal, err)
			}
		}
		if got, _ := intent(t, st); got != 0 {
			t.Fatalf("a refused order wrote intent %d", got)
		}
	})
}
