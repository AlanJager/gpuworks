package servicelevel

import (
	"testing"
	"time"
)

func preset(t *testing.T, name string) Policy {
	t.Helper()
	p, ok := PresetPolicy(name)
	if !ok {
		t.Fatalf("no preset %q", name)
	}
	return p.Normalize()
}

func TestYields(t *testing.T) {
	never, online, shared, offline := preset(t, PresetResident), preset(t, PresetOnline), preset(t, PresetShared), preset(t, PresetOffline)
	busy := func(p Policy, awake time.Duration) Holder { return Holder{Policy: p, AwakeFor: awake} }
	idle := func(p Policy, idle time.Duration) Holder {
		return Holder{Policy: p, AwakeFor: time.Hour, IdleFor: idle}
	}
	// A custom idle policy: gives its cards up after 30 s idle.
	quick := online
	quick.IdleSec = 30
	for _, c := range []struct {
		name   string
		holder Holder
		waiter Waiter
		want   Reason
		yields bool
	}{
		{"never does not yield, even idle", idle(never, 24*time.Hour), Waiter{Policy: online}, "", false},
		{"never with auto sleep still does not yield", idle(Policy{Yield: Never, AutoSleep: true, IdleSec: 30, SleepLevel: LevelAuto, HoldMaxSec: 120}, 24*time.Hour), Waiter{Policy: online}, "", false},
		{"an unknown policy reads as never", idle(Policy{Yield: "gold"}, 24*time.Hour), Waiter{Policy: online}, "", false},
		{"busy idle keeps its cards from anytime", busy(online, time.Hour), Waiter{Policy: offline, Waited: time.Hour}, "", false},
		{"busy idle keeps its cards from never", busy(online, time.Hour), Waiter{Policy: never, Waited: time.Hour}, "", false},
		{"idle for its idle time gives way", idle(online, 5*time.Minute), Waiter{Policy: offline}, ReasonIdle, true},
		{"idle for less does not", idle(online, 4*time.Minute), Waiter{Policy: offline}, "", false},
		{"idle follows its idle_sec", idle(quick, 30*time.Second), Waiter{Policy: offline}, ReasonIdle, true},
		{"busy anytime yields to idle at once", busy(offline, time.Second), Waiter{Policy: online}, ReasonRank, true},
		{"busy anytime yields to timeslice", busy(offline, time.Second), Waiter{Policy: shared}, ReasonRank, true},
		{"busy anytime yields to a never one waking", busy(offline, time.Second), Waiter{Policy: never}, ReasonRank, true},
		{"busy anytime keeps its cards from another anytime", busy(offline, time.Hour), Waiter{Policy: offline, Waited: time.Hour}, "", false},
		{"anytime idle 29 s keeps them from an equal", idle(offline, 29*time.Second), Waiter{Policy: offline}, "", false},
		{"anytime idle 30 s gives way to an equal", idle(offline, 30*time.Second), Waiter{Policy: offline}, ReasonIdle, true},
		{"anytime idle time is not idle_sec", idle(Policy{Yield: Anytime, AutoSleep: true, IdleSec: 3600, SleepLevel: LevelAuto, Unfinished: UnfinishedAbort, HoldMaxSec: 60}, 30*time.Second), Waiter{Policy: offline}, ReasonIdle, true},
		{"busy timeslice yields to idle", busy(shared, time.Second), Waiter{Policy: online}, ReasonRank, true},
		{"busy timeslice keeps its cards from anytime", busy(shared, time.Hour), Waiter{Policy: offline, Waited: time.Hour}, "", false},
		{"timeslice idle a minute gives way", idle(shared, time.Minute), Waiter{Policy: offline}, ReasonIdle, true},
		{"timeslice before its turn keeps its cards", busy(shared, 119*time.Second), Waiter{Policy: shared, Waited: time.Hour}, "", false},
		{"timeslice after its turn gives way to an equal that waited", busy(shared, 2*time.Minute), Waiter{Policy: shared, Waited: 30 * time.Second}, ReasonTurn, true},
		{"timeslice after its turn keeps its cards from an equal that just came", busy(shared, time.Hour), Waiter{Policy: shared, Waited: 29 * time.Second}, "", false},
		{"a slow switch lengthens the turn", Holder{Policy: shared, AwakeFor: 4 * time.Minute, SwitchTime: 30 * time.Second}, Waiter{Policy: shared, Waited: time.Hour}, "", false},
		{"a slow switch's turn ends", Holder{Policy: shared, AwakeFor: 5 * time.Minute, SwitchTime: 30 * time.Second}, Waiter{Policy: shared, Waited: time.Hour}, ReasonTurn, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, yields := Yields(c.holder, c.waiter)
			if got != c.want || yields != c.yields {
				t.Fatalf("Yields = %q, %v; want %q, %v", got, yields, c.want, c.yields)
			}
		})
	}
	custom := shared
	custom.MinServeSec = 10
	if r, ok := Yields(busy(custom, 10*time.Second), Waiter{Policy: shared, Waited: time.Minute}); !ok || r != ReasonTurn {
		t.Fatalf("a shorter min_serve_sec ends the turn sooner: %q %v", r, ok)
	}
}

func TestSharesCardsWith(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{Idle, Idle, false},
		{Never, Anytime, false},
		{Anytime, Never, false},
		{Never, Never, false},
		{Idle, TimeSlice, true},
		{Idle, Anytime, true},
		{TimeSlice, TimeSlice, true},
		{Anytime, Anytime, true},
		{TimeSlice, Idle, true},
	} {
		if got := SharesCardsWith(c.a, c.b); got != c.want {
			t.Errorf("SharesCardsWith(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPresetsAreDerivedFromTheParameters(t *testing.T) {
	for _, name := range Presets {
		p, ok := PresetPolicy(name)
		if !ok || PresetOf(p) != name || PresetOf(p.Normalize()) != name {
			t.Errorf("preset %s: %+v ok=%v derives %s", name, p, ok, PresetOf(p))
		}
		if v := p.Validate(); v != nil {
			t.Errorf("preset %s is invalid: %+v", name, v)
		}
		if !ValidPreset(name) {
			t.Errorf("%s is not a valid preset", name)
		}
	}
	if ValidPreset(PresetCustom) || ValidPreset("") {
		t.Fatal("custom is derived, not chosen")
	}
	if _, ok := PresetPolicy(PresetCustom); ok {
		t.Fatal("custom has no parameters")
	}
	online := preset(t, PresetOnline)
	for name, edit := range map[string]func(*Policy){
		"idle time":   func(p *Policy) { p.IdleSec = 30 },
		"auto sleep":  func(p *Policy) { p.AutoSleep = true },
		"sleep level": func(p *Policy) { p.SleepLevel = LevelL2 },
		"hold":        func(p *Policy) { p.HoldMaxSec = 121 },
		"policy":      func(p *Policy) { p.Yield = TimeSlice },
	} {
		p := online
		edit(&p)
		if got := PresetOf(p); got != PresetCustom {
			t.Errorf("online with a different %s = %s, want custom", name, got)
		}
	}
	// A parameter that does not apply cannot make a preset custom.
	resident := Policy{Yield: Never, IdleSec: 77, SleepLevel: LevelL2, MinServeSec: 5, Unfinished: UnfinishedAbort, HoldMaxSec: 9}
	if PresetOf(resident) != PresetResident {
		t.Fatalf("a never policy with leftover parameters = %s", PresetOf(resident))
	}
	// Auto sleep on a never policy makes the idle time and level apply.
	if got := PresetOf(Policy{Yield: Never, AutoSleep: true, IdleSec: 300, SleepLevel: LevelAuto, HoldMaxSec: 120}); got != PresetCustom {
		t.Fatalf("resident with auto sleep = %s, want custom", got)
	}
	if PresetOf(Policy{Yield: "gold"}) != PresetCustom {
		t.Fatal("an unknown policy is not a preset")
	}
}

func TestNormalizeClearsWhatDoesNotApply(t *testing.T) {
	full := Policy{Yield: Never, IdleSec: 60, SleepLevel: LevelL1, MinServeSec: 5, Unfinished: UnfinishedWait, UnfinishedWaitSec: 9, HoldMaxSec: 50}
	if got := full.Normalize(); got != (Policy{Yield: Never}) {
		t.Errorf("never = %+v", got)
	}
	full.AutoSleep = true
	if got := full.Normalize(); got != (Policy{Yield: Never, AutoSleep: true, IdleSec: 60, SleepLevel: LevelL1, HoldMaxSec: 50}) {
		t.Errorf("never with auto sleep = %+v", got)
	}
	full.Yield, full.AutoSleep = Idle, false
	if got := full.Normalize(); got != (Policy{Yield: Idle, IdleSec: 60, SleepLevel: LevelL1, HoldMaxSec: 50}) {
		t.Errorf("idle = %+v", got)
	}
	full.Yield = TimeSlice
	if got := full.Normalize(); got != full {
		t.Errorf("timeslice keeps all = %+v", got)
	}
	full.Yield, full.Unfinished = Anytime, UnfinishedAbort
	if got := full.Normalize(); got != (Policy{Yield: Anytime, SleepLevel: LevelL1, Unfinished: UnfinishedAbort, HoldMaxSec: 50}) {
		t.Errorf("anytime = %+v", got)
	}
	if got := (Policy{Yield: TimeSlice, Unfinished: UnfinishedWaitUnbounded, UnfinishedWaitSec: 9}).Normalize(); got.UnfinishedWaitSec != 0 || got.SleepLevel != LevelAuto {
		t.Errorf("unbounded wait keeps a bound or sleep level is not auto: %+v", got)
	}
	if got := (Policy{}).Normalize(); got.Yield != Never {
		t.Errorf("the zero policy = %+v", got)
	}
}

func TestApplies(t *testing.T) {
	for _, c := range []struct {
		name string
		p    Policy
		want Applicable
	}{
		{"never", Policy{Yield: Never}, Applicable{}},
		{"never with auto sleep", Policy{Yield: Never, AutoSleep: true}, Applicable{IdleSec: true, SleepLevel: true, HoldMaxSec: true}},
		{"idle", Policy{Yield: Idle}, Applicable{IdleSec: true, SleepLevel: true, HoldMaxSec: true}},
		{"timeslice waiting", Policy{Yield: TimeSlice, Unfinished: UnfinishedWait}, Applicable{IdleSec: true, SleepLevel: true, MinServeSec: true, Unfinished: true, UnfinishedWaitSec: true, HoldMaxSec: true}},
		{"timeslice unbounded", Policy{Yield: TimeSlice, Unfinished: UnfinishedWaitUnbounded}, Applicable{IdleSec: true, SleepLevel: true, MinServeSec: true, Unfinished: true, HoldMaxSec: true}},
		{"anytime", Policy{Yield: Anytime, Unfinished: UnfinishedAbort}, Applicable{SleepLevel: true, Unfinished: true, HoldMaxSec: true}},
		{"anytime with auto sleep", Policy{Yield: Anytime, AutoSleep: true, Unfinished: UnfinishedAbort}, Applicable{IdleSec: true, SleepLevel: true, Unfinished: true, HoldMaxSec: true}},
	} {
		if got := c.p.Applies(); got != c.want {
			t.Errorf("%s: applies %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestParseFillsDefaultsAndValidates(t *testing.T) {
	yes, no := true, false
	n := func(v int) *int { return &v }
	// Only a policy: the preset's parameters.
	for yield, want := range map[string]string{Never: PresetResident, Idle: PresetOnline, TimeSlice: PresetShared, Anytime: PresetOffline} {
		p, v := Parse(yield, Overrides{})
		if v != nil || PresetOf(p) != want {
			t.Errorf("%s alone = %+v violation=%+v preset=%s, want %s", yield, p, v, PresetOf(p), want)
		}
	}
	// Auto sleep on never starts from a 300 s idle time and the hold of a sleeper.
	p, v := Parse(Never, Overrides{AutoSleep: &yes})
	if v != nil || p != (Policy{Yield: Never, AutoSleep: true, IdleSec: 300, SleepLevel: LevelAuto, HoldMaxSec: 120}) {
		t.Errorf("never + auto sleep = %+v %+v", p, v)
	}
	p, v = Parse(TimeSlice, Overrides{IdleSec: n(30), Unfinished: UnfinishedWaitUnbounded, AutoSleep: &no, SleepLevel: LevelL1})
	if v != nil || p.IdleSec != 30 || p.Unfinished != UnfinishedWaitUnbounded || p.UnfinishedWaitSec != 0 || p.SleepLevel != LevelL1 || p.MinServeSec != 120 {
		t.Errorf("timeslice custom = %+v %+v", p, v)
	}
	p, v = Parse(Anytime, Overrides{Unfinished: UnfinishedWait})
	if v != nil || p.UnfinishedWaitSec != 30 {
		t.Errorf("anytime wait defaults to 30 s: %+v %+v", p, v)
	}
	// Parameters that do not apply are ignored, even out of range.
	p, v = Parse(Idle, Overrides{MinServeSec: n(99999), UnfinishedWaitSec: n(0), Unfinished: UnfinishedAbort})
	if v != nil || PresetOf(p) != PresetOnline {
		t.Errorf("inapplicable parameters were not ignored: %+v %+v", p, v)
	}

	for _, c := range []struct {
		name  string
		yield string
		o     Overrides
		want  Violation
	}{
		{"unknown policy", "gold", Overrides{}, Violation{Field: "policy", Reason: ReasonValue}},
		{"unknown sleep level", Never, Overrides{SleepLevel: "l3"}, Violation{Field: "sleep_level", Reason: ReasonValue}},
		{"unknown unfinished", Never, Overrides{Unfinished: "sometimes"}, Violation{Field: "unfinished", Reason: ReasonValue}},
		{"idle time under 30 s", Idle, Overrides{IdleSec: n(29)}, Violation{Field: "idle_sec", Reason: ReasonRange, Min: 30, Max: 7 * 24 * 3600}},
		{"idle time over 7 d", TimeSlice, Overrides{IdleSec: n(7*24*3600 + 1)}, Violation{Field: "idle_sec", Reason: ReasonRange, Min: 30, Max: 7 * 24 * 3600}},
		{"idle time under 30 s applies with auto sleep", Never, Overrides{AutoSleep: &yes, IdleSec: n(10)}, Violation{Field: "idle_sec", Reason: ReasonRange, Min: 30, Max: 7 * 24 * 3600}},
		{"min serve over an hour", TimeSlice, Overrides{MinServeSec: n(3601)}, Violation{Field: "min_serve_sec", Reason: ReasonRange, Min: 0, Max: 3600}},
		{"min serve negative", TimeSlice, Overrides{MinServeSec: n(-1)}, Violation{Field: "min_serve_sec", Reason: ReasonRange, Min: 0, Max: 3600}},
		{"unfinished wait of 0", TimeSlice, Overrides{Unfinished: UnfinishedWait, UnfinishedWaitSec: n(0)}, Violation{Field: "unfinished_wait_sec", Reason: ReasonRange, Min: 1, Max: 600}},
		{"unfinished wait over 600", Anytime, Overrides{Unfinished: UnfinishedWait, UnfinishedWaitSec: n(601)}, Violation{Field: "unfinished_wait_sec", Reason: ReasonRange, Min: 1, Max: 600}},
		{"hold under 10 s", Idle, Overrides{HoldMaxSec: n(9)}, Violation{Field: "hold_max_sec", Reason: ReasonRange, Min: 10, Max: 1800}},
		{"hold over 30 min", Anytime, Overrides{HoldMaxSec: n(1801)}, Violation{Field: "hold_max_sec", Reason: ReasonRange, Min: 10, Max: 1800}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, v := Parse(c.yield, c.o); v == nil || *v != c.want {
				t.Fatalf("violation = %+v, want %+v", v, c.want)
			}
		})
	}
	// The bounds themselves are valid.
	for _, o := range []Overrides{{IdleSec: n(30)}, {IdleSec: n(7 * 24 * 3600)}, {HoldMaxSec: n(10)}, {HoldMaxSec: n(1800)}} {
		if _, v := Parse(Idle, o); v != nil {
			t.Errorf("%+v refused: %+v", o, v)
		}
	}
	for _, o := range []Overrides{{MinServeSec: n(0)}, {MinServeSec: n(3600)}, {Unfinished: UnfinishedWait, UnfinishedWaitSec: n(1)}, {Unfinished: UnfinishedWait, UnfinishedWaitSec: n(600)}} {
		if _, v := Parse(TimeSlice, o); v != nil {
			t.Errorf("%+v refused: %+v", o, v)
		}
	}
}

func TestPolicyAccessors(t *testing.T) {
	never, online, shared, offline := preset(t, PresetResident), preset(t, PresetOnline), preset(t, PresetShared), preset(t, PresetOffline)
	if never.Rank() <= online.Rank() || online.Rank() <= shared.Rank() || shared.Rank() <= offline.Rank() {
		t.Fatal("ranks out of order")
	}
	if (Policy{Yield: "gold"}).Rank() != never.Rank() {
		t.Fatal("an unknown policy reads as never")
	}
	if Turn(shared, 0) != 2*time.Minute || Turn(shared, time.Minute) != 10*time.Minute {
		t.Fatal("turn does not follow the switch time")
	}
	if never.HoldMax() != 2*time.Minute || online.HoldMax() != 2*time.Minute || shared.HoldMax() != 10*time.Minute || offline.HoldMax() != 30*time.Minute {
		t.Fatal("hold times")
	}
	if online.Preemptible() || !shared.Preemptible() || !offline.Preemptible() || never.Preemptible() {
		t.Fatal("preemptible")
	}
	if abort, _ := offline.Drain(); !abort {
		t.Fatal("offline cuts requests off")
	}
	if abort, limit := shared.Drain(); abort || limit != 30*time.Second {
		t.Fatalf("shared drains for 30 s: %v %v", abort, limit)
	}
	unbounded := shared
	unbounded.Unfinished = UnfinishedWaitUnbounded
	if abort, limit := unbounded.Normalize().Drain(); abort || limit != 0 {
		t.Fatalf("unbounded drain = %v %v", abort, limit)
	}
	if never.IdleRelease() != 0 || online.IdleRelease() != 5*time.Minute || shared.IdleRelease() != time.Minute || offline.IdleRelease() != AnytimeIdle {
		t.Fatal("idle release")
	}
	if (Policy{}).SleepLevelPref() != LevelAuto {
		t.Fatal("no sleep level reads as auto")
	}
	if ValidYield("") || ValidYield("gold") || !ValidYield(Anytime) {
		t.Fatal("yield validity")
	}
}
