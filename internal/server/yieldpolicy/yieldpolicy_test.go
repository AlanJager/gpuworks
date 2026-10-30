package yieldpolicy

import (
	"testing"
	"time"
)

func defaults(t *testing.T, yield string) Policy {
	t.Helper()
	p, v := Parse(yield, Overrides{})
	if v != nil {
		t.Fatalf("defaults of %s are invalid: %+v", yield, v)
	}
	return p
}

func TestYields(t *testing.T) {
	never, idle, slice, anytime := defaults(t, Never), defaults(t, Idle), defaults(t, TimeSlice), defaults(t, Anytime)
	busy := func(p Policy, awake time.Duration) Holder { return Holder{Policy: p, AwakeFor: awake, Busy: true} }
	quiet := func(p Policy, idle time.Duration) Holder {
		return Holder{Policy: p, AwakeFor: time.Hour, IdleFor: idle}
	}
	// A quicker idle policy: gives its cards up after 15 s idle.
	quick := idle
	quick.IdleSec = 15
	for _, c := range []struct {
		name   string
		holder Holder
		waiter Waiter
		want   Reason
		yields bool
	}{
		{"never does not yield, even idle", quiet(never, 24*time.Hour), Waiter{Policy: idle}, "", false},
		{"an unknown policy reads as never", quiet(Policy{Yield: "gold"}, 24*time.Hour), Waiter{Policy: idle}, "", false},
		{"busy idle keeps its cards from anytime", busy(idle, time.Hour), Waiter{Policy: anytime}, "", false},
		{"busy idle keeps its cards from never", busy(idle, time.Hour), Waiter{Policy: never}, "", false},
		{"busy idle keeps its cards from another idle", busy(idle, time.Hour), Waiter{Policy: idle}, "", false},
		{"idle for its idle time gives way", quiet(idle, time.Minute), Waiter{Policy: anytime}, ReasonIdle, true},
		{"idle for its idle time gives way to another idle", quiet(idle, time.Minute), Waiter{Policy: idle}, ReasonIdle, true},
		{"idle for less does not", quiet(idle, 59*time.Second), Waiter{Policy: anytime}, "", false},
		{"idle follows its idle_sec", quiet(quick, 15*time.Second), Waiter{Policy: anytime}, ReasonIdle, true},
		{"busy anytime yields to idle at once", busy(anytime, time.Second), Waiter{Policy: idle}, ReasonRank, true},
		{"busy anytime yields to timeslice", busy(anytime, time.Second), Waiter{Policy: slice}, ReasonRank, true},
		{"busy anytime yields to a never one waking", busy(anytime, time.Second), Waiter{Policy: never}, ReasonRank, true},
		{"busy anytime keeps its cards from another anytime", busy(anytime, time.Hour), Waiter{Policy: anytime}, "", false},
		{"anytime idle 29 s keeps them from an equal", quiet(anytime, 29*time.Second), Waiter{Policy: anytime}, "", false},
		{"anytime idle 30 s gives way to an equal", quiet(anytime, 30*time.Second), Waiter{Policy: anytime}, ReasonIdle, true},
		{"busy timeslice yields to idle", busy(slice, time.Second), Waiter{Policy: idle}, ReasonRank, true},
		{"busy timeslice keeps its cards from anytime", busy(slice, time.Hour), Waiter{Policy: anytime}, "", false},
		{"timeslice idle 30 s gives way", quiet(slice, 30*time.Second), Waiter{Policy: anytime}, ReasonIdle, true},
		{"timeslice before its turn ends keeps its cards", busy(slice, 119*time.Second), Waiter{Policy: slice}, "", false},
		{"timeslice at the end of its turn gives way to an equal", busy(slice, 2*time.Minute), Waiter{Policy: slice}, ReasonTurn, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, yields := Yields(c.holder, c.waiter)
			if got != c.want || yields != c.yields {
				t.Fatalf("Yields = %q, %v; want %q, %v", got, yields, c.want, c.yields)
			}
		})
	}
	short := slice
	short.MaxServeSec = 30
	if r, ok := Yields(busy(short, 30*time.Second), Waiter{Policy: slice}); !ok || r != ReasonTurn {
		t.Fatalf("a shorter max_serve_sec ends the turn sooner: %q %v", r, ok)
	}
}

func TestDue(t *testing.T) {
	never, idle, slice, anytime := defaults(t, Never), defaults(t, Idle), defaults(t, TimeSlice), defaults(t, Anytime)
	for _, c := range []struct {
		name   string
		holder Holder
		waiter Waiter
		due    time.Duration
		ok     bool
	}{
		{"never is never due", Holder{Policy: never, IdleFor: time.Hour}, Waiter{Policy: idle}, 0, false},
		{"an idle holder is due when its idle time is up", Holder{Policy: idle, IdleFor: 20 * time.Second}, Waiter{Policy: anytime}, 40 * time.Second, true},
		{"a busy idle holder waits on its requests, which no clock tells", Holder{Policy: idle, Busy: true}, Waiter{Policy: anytime}, 0, false},
		{"one that must yield now is due now", Holder{Policy: anytime, Busy: true}, Waiter{Policy: idle}, 0, true},
		{"a busy timeslice holder is due at the end of its turn", Holder{Policy: slice, Busy: true, AwakeFor: 100 * time.Second}, Waiter{Policy: slice}, 20 * time.Second, true},
		{"the idle time ends first", Holder{Policy: slice, AwakeFor: 10 * time.Second, IdleFor: 25 * time.Second}, Waiter{Policy: slice}, 5 * time.Second, true},
		{"the turn ends first", Holder{Policy: slice, AwakeFor: 118 * time.Second}, Waiter{Policy: slice}, 2 * time.Second, true},
		{"a turn is no clock for a lower waiter", Holder{Policy: slice, Busy: true, AwakeFor: 100 * time.Second}, Waiter{Policy: anytime}, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			due, ok := Due(c.holder, c.waiter)
			if due != c.due || ok != c.ok {
				t.Fatalf("Due = %v, %v; want %v, %v", due, ok, c.due, c.ok)
			}
		})
	}
}

func TestSharesCardsWith(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{Idle, Idle, true},
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
	if !IdleNeighbours(Idle, Idle) || IdleNeighbours(Idle, TimeSlice) || IdleNeighbours(Never, Idle) {
		t.Fatal("only two idle deployments are idle neighbours")
	}
}

func TestDefaults(t *testing.T) {
	for _, c := range []struct {
		yield string
		want  Policy
	}{
		{Never, Policy{Yield: Never}},
		{Idle, Policy{Yield: Idle, IdleSec: 60, SleepLevel: LevelAuto, HoldMaxSec: 300}},
		{TimeSlice, Policy{Yield: TimeSlice, IdleSec: 30, SleepLevel: LevelAuto, MaxServeSec: 120, Unfinished: UnfinishedWait, UnfinishedWaitSec: 30, HoldMaxSec: 600}},
		{Anytime, Policy{Yield: Anytime, IdleSec: 30, SleepLevel: LevelAuto, Unfinished: UnfinishedAbort, HoldMaxSec: 3600}},
	} {
		if got := defaults(t, c.yield); got != c.want {
			t.Errorf("defaults of %s = %+v, want %+v", c.yield, got, c.want)
		}
	}
}

func TestNormalizeClearsWhatDoesNotApply(t *testing.T) {
	full := Policy{Yield: Never, IdleSec: 60, SleepLevel: LevelL1, MaxServeSec: 50, Unfinished: UnfinishedWait, UnfinishedWaitSec: 9, HoldMaxSec: 50}
	if got := full.Normalize(); got != (Policy{Yield: Never}) {
		t.Errorf("never = %+v", got)
	}
	full.Yield = Idle
	if got := full.Normalize(); got != (Policy{Yield: Idle, IdleSec: 60, SleepLevel: LevelL1, HoldMaxSec: 50}) {
		t.Errorf("idle = %+v", got)
	}
	full.Yield = TimeSlice
	if got := full.Normalize(); got != full {
		t.Errorf("timeslice keeps all = %+v", got)
	}
	full.Yield, full.Unfinished = Anytime, UnfinishedAbort
	if got := full.Normalize(); got != (Policy{Yield: Anytime, IdleSec: 60, SleepLevel: LevelL1, Unfinished: UnfinishedAbort, HoldMaxSec: 50}) {
		t.Errorf("anytime = %+v", got)
	}
	if got := (Policy{Yield: TimeSlice}).Normalize(); got.SleepLevel != LevelAuto {
		t.Errorf("sleep level is not auto: %+v", got)
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
		{"idle", Policy{Yield: Idle}, Applicable{IdleSec: true, SleepLevel: true, HoldMaxSec: true}},
		{"timeslice waiting", Policy{Yield: TimeSlice, Unfinished: UnfinishedWait}, Applicable{IdleSec: true, SleepLevel: true, MaxServeSec: true, Unfinished: true, UnfinishedWaitSec: true, HoldMaxSec: true}},
		{"timeslice cutting off", Policy{Yield: TimeSlice, Unfinished: UnfinishedAbort}, Applicable{IdleSec: true, SleepLevel: true, MaxServeSec: true, Unfinished: true, HoldMaxSec: true}},
		{"anytime", Policy{Yield: Anytime, Unfinished: UnfinishedAbort}, Applicable{IdleSec: true, SleepLevel: true, Unfinished: true, HoldMaxSec: true}},
		{"anytime waiting", Policy{Yield: Anytime, Unfinished: UnfinishedWait}, Applicable{IdleSec: true, SleepLevel: true, Unfinished: true, UnfinishedWaitSec: true, HoldMaxSec: true}},
	} {
		if got := c.p.Applies(); got != c.want {
			t.Errorf("%s: applies %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestParseFillsDefaultsAndValidates(t *testing.T) {
	n := func(v int) *int { return &v }
	p, v := Parse(TimeSlice, Overrides{IdleSec: n(15), Unfinished: UnfinishedAbort, SleepLevel: LevelL1, MaxServeSec: n(30)})
	if v != nil || p != (Policy{Yield: TimeSlice, IdleSec: 15, SleepLevel: LevelL1, MaxServeSec: 30, Unfinished: UnfinishedAbort, HoldMaxSec: 600}) {
		t.Errorf("timeslice stated = %+v %+v", p, v)
	}
	p, v = Parse(Anytime, Overrides{Unfinished: UnfinishedWait})
	if v != nil || p.UnfinishedWaitSec != 30 {
		t.Errorf("anytime wait defaults to 30 s: %+v %+v", p, v)
	}
	// Parameters that do not apply are ignored, even out of range.
	p, v = Parse(Idle, Overrides{MaxServeSec: n(99999), UnfinishedWaitSec: n(0), Unfinished: UnfinishedAbort})
	if v != nil || p != defaults(t, Idle) {
		t.Errorf("inapplicable parameters were not ignored: %+v %+v", p, v)
	}
	p, v = Parse(Never, Overrides{IdleSec: n(1), HoldMaxSec: n(1)})
	if v != nil || p != (Policy{Yield: Never}) {
		t.Errorf("never takes no parameters: %+v %+v", p, v)
	}

	week := 7 * 24 * 3600
	for _, c := range []struct {
		name  string
		yield string
		o     Overrides
		want  Violation
	}{
		{"unknown policy", "gold", Overrides{}, Violation{Field: "policy", Reason: ReasonValue}},
		{"unknown sleep level", Never, Overrides{SleepLevel: "l3"}, Violation{Field: "sleep_level", Reason: ReasonValue}},
		{"unknown unfinished", Never, Overrides{Unfinished: "sometimes"}, Violation{Field: "unfinished", Reason: ReasonValue}},
		{"the unbounded wait is gone", TimeSlice, Overrides{Unfinished: "wait_unbounded"}, Violation{Field: "unfinished", Reason: ReasonValue}},
		{"idle time under 15 s", Idle, Overrides{IdleSec: n(14)}, Violation{Field: "idle_sec", Reason: ReasonRange, Min: 15, Max: week}},
		{"idle time over 7 d", Idle, Overrides{IdleSec: n(week + 1)}, Violation{Field: "idle_sec", Reason: ReasonRange, Min: 15, Max: week}},
		{"anytime idle time under 15 s", Anytime, Overrides{IdleSec: n(14)}, Violation{Field: "idle_sec", Reason: ReasonRange, Min: 15, Max: week}},
		{"timeslice idle time past its turn", TimeSlice, Overrides{IdleSec: n(121)}, Violation{Field: "idle_sec", Reason: ReasonRange, Min: 15, Max: 120}},
		{"max serve under 30 s", TimeSlice, Overrides{MaxServeSec: n(29), IdleSec: n(15)}, Violation{Field: "max_serve_sec", Reason: ReasonRange, Min: 30, Max: 24 * 3600}},
		{"max serve over a day", TimeSlice, Overrides{MaxServeSec: n(24*3600 + 1)}, Violation{Field: "max_serve_sec", Reason: ReasonRange, Min: 30, Max: 24 * 3600}},
		{"unfinished wait of 0", TimeSlice, Overrides{Unfinished: UnfinishedWait, UnfinishedWaitSec: n(0)}, Violation{Field: "unfinished_wait_sec", Reason: ReasonRange, Min: 1, Max: 600}},
		{"unfinished wait over 600", Anytime, Overrides{Unfinished: UnfinishedWait, UnfinishedWaitSec: n(601)}, Violation{Field: "unfinished_wait_sec", Reason: ReasonRange, Min: 1, Max: 600}},
		{"hold under 10 s", Idle, Overrides{HoldMaxSec: n(9)}, Violation{Field: "hold_max_sec", Reason: ReasonRange, Min: 10, Max: 3600}},
		{"hold over an hour", Anytime, Overrides{HoldMaxSec: n(3601)}, Violation{Field: "hold_max_sec", Reason: ReasonRange, Min: 10, Max: 3600}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, v := Parse(c.yield, c.o); v == nil || *v != c.want {
				t.Fatalf("violation = %+v, want %+v", v, c.want)
			}
		})
	}
	// The bounds themselves are valid.
	for _, o := range []Overrides{{IdleSec: n(15)}, {IdleSec: n(week)}, {HoldMaxSec: n(10)}, {HoldMaxSec: n(3600)}} {
		if _, v := Parse(Idle, o); v != nil {
			t.Errorf("%+v refused: %+v", o, v)
		}
	}
	for _, o := range []Overrides{{MaxServeSec: n(30), IdleSec: n(30)}, {MaxServeSec: n(24 * 3600)}, {Unfinished: UnfinishedWait, UnfinishedWaitSec: n(1)}, {Unfinished: UnfinishedWait, UnfinishedWaitSec: n(600)}} {
		if _, v := Parse(TimeSlice, o); v != nil {
			t.Errorf("%+v refused: %+v", o, v)
		}
	}
}

func TestPolicyAccessors(t *testing.T) {
	never, idle, slice, anytime := defaults(t, Never), defaults(t, Idle), defaults(t, TimeSlice), defaults(t, Anytime)
	if never.Rank() <= idle.Rank() || idle.Rank() <= slice.Rank() || slice.Rank() <= anytime.Rank() {
		t.Fatal("ranks out of order")
	}
	if (Policy{Yield: "gold"}).Rank() != never.Rank() {
		t.Fatal("an unknown policy reads as never")
	}
	if slice.Turn() != 2*time.Minute {
		t.Fatal("turn is max_serve_sec")
	}
	if never.HoldMax() != DefaultHoldMax || idle.HoldMax() != 5*time.Minute || slice.HoldMax() != 10*time.Minute || anytime.HoldMax() != time.Hour {
		t.Fatal("hold times")
	}
	if idle.Preemptible() || !slice.Preemptible() || !anytime.Preemptible() || never.Preemptible() {
		t.Fatal("preemptible")
	}
	if abort, _ := anytime.Drain(); !abort {
		t.Fatal("anytime cuts requests off")
	}
	if abort, limit := slice.Drain(); abort || limit != 30*time.Second {
		t.Fatalf("timeslice drains for 30 s: %v %v", abort, limit)
	}
	if never.IdleRelease() != 0 || idle.IdleRelease() != time.Minute || slice.IdleRelease() != 30*time.Second || anytime.IdleRelease() != 30*time.Second {
		t.Fatal("idle release")
	}
	if (Policy{Yield: Idle}).Normalize().SleepLevel != LevelAuto {
		t.Fatal("no sleep level reads as auto")
	}
	// Ranks follow Policies, highest first.
	for i, yield := range Policies {
		if got := (Policy{Yield: yield}).Rank(); got != len(Policies)-i {
			t.Errorf("rank of %s = %d, want %d", yield, got, len(Policies)-i)
		}
	}
	if ValidYield("") || ValidYield("gold") || !ValidYield(Anytime) {
		t.Fatal("yield validity")
	}
}

// Cards given up for a replica waiting to be placed stay its against a
// sleeper that ranks no higher; one of higher rank takes them back.
func TestKeepsCardsAgainst(t *testing.T) {
	idle, slice, anytime := defaults(t, Idle), defaults(t, TimeSlice), defaults(t, Anytime)
	for _, c := range []struct {
		name            string
		waiter, sleeper Policy
		want            bool
	}{
		{"against a lower sleeper", idle, anytime, true},
		{"against an equal", slice, slice, true},
		{"not against a higher sleeper", anytime, idle, false},
	} {
		if got := KeepsCardsAgainst(c.waiter, c.sleeper); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTermsOf(t *testing.T) {
	never, idle, slice, anytime := defaults(t, Never), defaults(t, Idle), defaults(t, TimeSlice), defaults(t, Anytime)
	for _, c := range []struct {
		name           string
		holder, waiter Policy
		want           Terms
	}{
		{"never never gives way", never, idle, Terms{Never: true}},
		{"anytime gives way to idle at once", anytime, idle, Terms{AtOnce: true}},
		{"timeslice gives way to never at once", slice, never, Terms{AtOnce: true}},
		{"idle gives way once idle", idle, anytime, Terms{Idle: time.Minute}},
		{"anytime to anytime once idle", anytime, anytime, Terms{Idle: 30 * time.Second}},
		{"timeslice to timeslice once idle or at the end of its turn", slice, slice, Terms{Idle: 30 * time.Second, Turn: 2 * time.Minute}},
		{"timeslice to anytime once idle", slice, anytime, Terms{Idle: 30 * time.Second}},
	} {
		if got := TermsOf(c.holder, c.waiter); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}
