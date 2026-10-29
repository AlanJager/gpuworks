// Package servicelevel is how a deployment shares GPUs with others: its yield
// policy, the parameters that go with it, and the presets (the four service
// levels) those stand for. It is a leaf, read by the store (the columns'
// values), the gateway (how long a request may wait for a wake) and the
// residency arbiter (who may put whom to sleep).
//
// The rule every policy follows (docs/explanation/service-levels.md): a
// deployment waiting for its cards may have the one awake on them put to
// sleep when that one can be preempted and either ranks lower, has had its
// turn at the same rank, or has been idle long enough to give the cards up.
package servicelevel

import "time"

// Yield policies, stored in Deployment.YieldPolicy. The policy is the only
// thing the yield rule looks at to rank deployments.
const (
	// Never gives its cards to no other deployment. Every deployment from
	// before yield policies is never.
	Never = "never"
	// Idle gives its cards up only when another deployment waits for them
	// and it has been idle for IdleSec; it takes them back from timeslice
	// and anytime deployments as soon as a request comes.
	Idle = "idle"
	// TimeSlice takes turns with the other timeslice deployments on its
	// cards (each keeps them at least MinServeSec) and gives them up to
	// never and idle ones.
	TimeSlice = "timeslice"
	// Anytime gives its cards up to every higher-ranked waiter at once and
	// resumes when the cards are free again.
	Anytime = "anytime"
)

// ValidYield says p is one of the yield policies.
func ValidYield(p string) bool {
	switch p {
	case Never, Idle, TimeSlice, Anytime:
		return true
	}
	return false
}

// Presets: the four service levels, each a full set of parameters
// (PresetPolicy). A deployment whose parameters match none is Custom.
const (
	PresetResident = "resident"
	PresetOnline   = "online"
	PresetShared   = "shared"
	PresetOffline  = "offline"
	PresetCustom   = "custom"
)

// Presets are the presets a deployment can be set to, in display order.
var Presets = []string{PresetResident, PresetOnline, PresetShared, PresetOffline}

// ValidPreset says p is one of Presets (custom is derived, never chosen).
func ValidPreset(p string) bool {
	for _, name := range Presets {
		if p == name {
			return true
		}
	}
	return false
}

// Sleep level preferences for a sleep the platform chooses (SleepLevel).
const (
	// LevelAuto sleeps at L1, and at L2 when the host cannot hold the
	// weights an L1 sleep moves there.
	LevelAuto = "auto"
	LevelL1   = "l1"
	LevelL2   = "l2"
)

// ValidSleepLevel says l is one of the sleep level preferences.
func ValidSleepLevel(l string) bool { return l == LevelAuto || l == LevelL1 || l == LevelL2 }

// What becomes of the requests still running when a deployment gives its
// cards up (Unfinished).
const (
	// UnfinishedAbort cuts them off at once.
	UnfinishedAbort = "abort"
	// UnfinishedWait lets them run on for UnfinishedWaitSec, then cuts off
	// what is left.
	UnfinishedWait = "wait"
	// UnfinishedWaitUnbounded lets them run on however long they take.
	UnfinishedWaitUnbounded = "wait_unbounded"
)

// ValidUnfinished says u is one of the unfinished-request handlings.
func ValidUnfinished(u string) bool {
	return u == UnfinishedAbort || u == UnfinishedWait || u == UnfinishedWaitUnbounded
}

// Bounds of the parameters, in seconds.
const (
	IdleSecMin           = 30
	IdleSecMax           = 7 * 24 * 3600
	MinServeSecMax       = 3600
	UnfinishedWaitSecMin = 1
	UnfinishedWaitSecMax = 600
	HoldMaxSecMin        = 10
	HoldMaxSecMax        = 1800
)

// Fixed internal times of the yield rule that no parameter sets.
const (
	// AnytimeIdle is how long an anytime deployment must sit idle before it
	// gives its cards to a waiter (an equal one, in effect: a higher-ranked
	// waiter gets them at once).
	AnytimeIdle = 30 * time.Second
	// TurnWait is how long a timeslice waiter's oldest request must have
	// waited before the deployment that has had its turn gives way to it.
	TurnWait = 30 * time.Second
	// DefaultHoldMax is how long the gateway holds a request for a
	// deployment whose policy states no hold time.
	DefaultHoldMax = 2 * time.Minute
)

// SwitchMultiple sets a timeslice deployment's turn from the measured switch
// time: max(MinServeSec, SwitchMultiple × switch), so the time spent
// switching stays a small share of the time spent serving.
const SwitchMultiple = 10

// Policy is a deployment's yield policy and its parameters. A parameter that
// does not apply to the policy is its zero value (Normalize); which apply is
// in Applies.
type Policy struct {
	// Yield is one of Never, Idle, TimeSlice, Anytime.
	Yield string
	// AutoSleep puts the deployment to sleep after IdleSec without a
	// request even when no deployment waits for its cards.
	AutoSleep bool
	// IdleSec is how long without a request counts as idle: for the idle and
	// timeslice policies before giving the cards up to a waiter, and for any
	// policy before an auto sleep.
	IdleSec int
	// SleepLevel is the level of every sleep the platform chooses for the
	// deployment (a yield, an auto sleep): LevelAuto, LevelL1 or LevelL2.
	SleepLevel string
	// MinServeSec is the least a timeslice deployment keeps its cards once
	// woken before an equal may take them.
	MinServeSec int
	// Unfinished is what becomes of the requests still running when a
	// timeslice or anytime deployment gives its cards up; UnfinishedWaitSec
	// is how long they may run on for UnfinishedWait.
	Unfinished        string
	UnfinishedWaitSec int
	// HoldMaxSec is how long the gateway holds a request for the deployment
	// while it wakes.
	HoldMaxSec int
}

// Which parameters apply to a policy.
func (p Policy) usesIdle() bool       { return p.Yield == Idle || p.Yield == TimeSlice || p.AutoSleep }
func (p Policy) sleeps() bool         { return p.Yield != Never || p.AutoSleep }
func (p Policy) usesMinServe() bool   { return p.Yield == TimeSlice }
func (p Policy) usesUnfinished() bool { return p.Yield == TimeSlice || p.Yield == Anytime }

// Applicable says which of the optional parameters apply, so a client shows
// only those: IdleSec, SleepLevel, MinServeSec, Unfinished (and
// UnfinishedWaitSec when it is UnfinishedWait) and HoldMaxSec.
type Applicable struct {
	IdleSec, SleepLevel, MinServeSec, Unfinished, UnfinishedWaitSec, HoldMaxSec bool
}

// Applies is which parameters apply to p.
func (p Policy) Applies() Applicable {
	return Applicable{
		IdleSec: p.usesIdle(), SleepLevel: p.sleeps(), MinServeSec: p.usesMinServe(),
		Unfinished: p.usesUnfinished(), UnfinishedWaitSec: p.usesUnfinished() && p.Unfinished == UnfinishedWait,
		HoldMaxSec: p.sleeps(),
	}
}

// Normalize is p with every parameter that does not apply cleared, and a
// sleep level that applies but is empty read as auto. A policy read from
// storage or built from a request is normalized, so equal policies compare
// equal and a preset is recognized by ==.
func (p Policy) Normalize() Policy {
	if p.Yield == "" {
		p.Yield = Never
	}
	if !p.usesIdle() {
		p.IdleSec = 0
	}
	if !p.sleeps() {
		p.SleepLevel, p.HoldMaxSec = "", 0
	} else if p.SleepLevel == "" {
		p.SleepLevel = LevelAuto
	}
	if !p.usesMinServe() {
		p.MinServeSec = 0
	}
	if !p.usesUnfinished() {
		p.Unfinished = ""
	}
	if p.Unfinished != UnfinishedWait {
		p.UnfinishedWaitSec = 0
	}
	return p
}

// PresetPolicy is the parameters of a preset (a name from Presets).
func PresetPolicy(name string) (Policy, bool) {
	switch name {
	case PresetResident:
		return Policy{Yield: Never}, true
	case PresetOnline:
		return Policy{Yield: Idle, IdleSec: 300, SleepLevel: LevelAuto, HoldMaxSec: 120}, true
	case PresetShared:
		return Policy{
			Yield: TimeSlice, IdleSec: 60, SleepLevel: LevelAuto, MinServeSec: 120,
			Unfinished: UnfinishedWait, UnfinishedWaitSec: 30, HoldMaxSec: 600,
		}, true
	case PresetOffline:
		return Policy{Yield: Anytime, SleepLevel: LevelAuto, Unfinished: UnfinishedAbort, HoldMaxSec: 1800}, true
	}
	return Policy{}, false
}

// PresetOf is the preset p stands for: the one whose parameters p matches
// exactly, else custom. It is derived, never stored.
func PresetOf(p Policy) string {
	p = p.Normalize()
	for _, name := range Presets {
		if preset, _ := PresetPolicy(name); preset.Normalize() == p {
			return name
		}
	}
	return PresetCustom
}

// Defaults is the full set of parameters a client that names only a policy
// (and some parameters) gets for the rest: the preset of the policy, with
// the values a policy's optional features (auto sleep, the unfinished wait)
// start from. Nothing here is normalized.
func Defaults(yield string) Policy {
	preset := map[string]string{Never: PresetResident, Idle: PresetOnline, TimeSlice: PresetShared, Anytime: PresetOffline}[yield]
	p, _ := PresetPolicy(preset)
	p.Yield = yield
	if p.IdleSec == 0 {
		p.IdleSec = 300
	}
	if p.SleepLevel == "" {
		p.SleepLevel = LevelAuto
	}
	if p.HoldMaxSec == 0 {
		p.HoldMaxSec = int(DefaultHoldMax / time.Second)
	}
	if p.UnfinishedWaitSec == 0 {
		p.UnfinishedWaitSec = 30
	}
	return p
}

// Overrides are the parameters a request states; nil/empty is not stated and
// takes the policy's default (Build).
type Overrides struct {
	AutoSleep         *bool
	IdleSec           *int
	SleepLevel        string
	MinServeSec       *int
	Unfinished        string
	UnfinishedWaitSec *int
	HoldMaxSec        *int
}

// Build is the policy a request states: yield with the defaults of that
// policy under the stated parameters, normalized. It does not validate.
func Build(yield string, o Overrides) Policy {
	p := Defaults(yield)
	if o.AutoSleep != nil {
		p.AutoSleep = *o.AutoSleep
	}
	if o.IdleSec != nil {
		p.IdleSec = *o.IdleSec
	}
	if o.SleepLevel != "" {
		p.SleepLevel = o.SleepLevel
	}
	if o.MinServeSec != nil {
		p.MinServeSec = *o.MinServeSec
	}
	if o.Unfinished != "" {
		p.Unfinished = o.Unfinished
	}
	if o.UnfinishedWaitSec != nil {
		p.UnfinishedWaitSec = *o.UnfinishedWaitSec
	}
	if o.HoldMaxSec != nil {
		p.HoldMaxSec = *o.HoldMaxSec
	}
	return p.Normalize()
}

// Parse is Build and Validate in one: the enumerations a request names are
// checked even where the parameter does not apply (a misspelt value is a
// mistake, not a thing to ignore), the ranges only where it does.
func Parse(yield string, o Overrides) (Policy, *Violation) {
	if !ValidYield(yield) {
		return Policy{}, &Violation{Field: "policy", Reason: ReasonValue}
	}
	if o.SleepLevel != "" && !ValidSleepLevel(o.SleepLevel) {
		return Policy{}, &Violation{Field: "sleep_level", Reason: ReasonValue}
	}
	if o.Unfinished != "" && !ValidUnfinished(o.Unfinished) {
		return Policy{}, &Violation{Field: "unfinished", Reason: ReasonValue}
	}
	p := Build(yield, o)
	if v := p.Validate(); v != nil {
		return Policy{}, v
	}
	return p, nil
}

// Violation is why a policy is refused: the request field, and either that
// its value is none of the allowed ones (Reason ReasonValue), out of range
// (ReasonRange, with Min and Max) or missing (ReasonRequired).
type Violation struct {
	Field    string
	Reason   string
	Min, Max int
}

// Reasons of a Violation.
const (
	ReasonValue    = "invalid_value"
	ReasonRange    = "out_of_range"
	ReasonRequired = "required"
)

// Validate checks p: the enumerations it states and the ranges of the
// parameters that apply to it. nil when it is valid.
func (p Policy) Validate() *Violation {
	if !ValidYield(p.Yield) {
		return &Violation{Field: "policy", Reason: ReasonValue}
	}
	if p.SleepLevel != "" && !ValidSleepLevel(p.SleepLevel) {
		return &Violation{Field: "sleep_level", Reason: ReasonValue}
	}
	if p.Unfinished != "" && !ValidUnfinished(p.Unfinished) {
		return &Violation{Field: "unfinished", Reason: ReasonValue}
	}
	apply := p.Applies()
	if apply.IdleSec && (p.IdleSec < IdleSecMin || p.IdleSec > IdleSecMax) {
		return &Violation{Field: "idle_sec", Reason: ReasonRange, Min: IdleSecMin, Max: IdleSecMax}
	}
	if apply.MinServeSec && (p.MinServeSec < 0 || p.MinServeSec > MinServeSecMax) {
		return &Violation{Field: "min_serve_sec", Reason: ReasonRange, Min: 0, Max: MinServeSecMax}
	}
	if apply.Unfinished && !ValidUnfinished(p.Unfinished) {
		return &Violation{Field: "unfinished", Reason: ReasonValue}
	}
	if apply.UnfinishedWaitSec && (p.UnfinishedWaitSec < UnfinishedWaitSecMin || p.UnfinishedWaitSec > UnfinishedWaitSecMax) {
		return &Violation{Field: "unfinished_wait_sec", Reason: ReasonRange, Min: UnfinishedWaitSecMin, Max: UnfinishedWaitSecMax}
	}
	if apply.HoldMaxSec && (p.HoldMaxSec < HoldMaxSecMin || p.HoldMaxSec > HoldMaxSecMax) {
		return &Violation{Field: "hold_max_sec", Reason: ReasonRange, Min: HoldMaxSecMin, Max: HoldMaxSecMax}
	}
	return nil
}

// Rank orders policies: a waiting deployment may preempt one of lower rank.
// An unknown policy reads as never.
func (p Policy) Rank() int {
	switch p.Yield {
	case Idle:
		return 3
	case TimeSlice:
		return 2
	case Anytime:
		return 1
	}
	return 4
}

// Preemptible says the arbiter may put the deployment to sleep for another
// while it is busy. One that is not is put to sleep only when idle, and a
// never one not at all.
func (p Policy) Preemptible() bool { return p.Yield == TimeSlice || p.Yield == Anytime }

// TakesTurns says deployments of this policy on the same cards take turns.
func (p Policy) TakesTurns() bool { return p.Yield == TimeSlice }

// IdleRelease is how long the deployment may sit with no request in flight
// or waiting before it gives its cards to a deployment waiting for them;
// zero never.
func (p Policy) IdleRelease() time.Duration {
	switch p.Yield {
	case Idle, TimeSlice:
		return time.Duration(p.IdleSec) * time.Second
	case Anytime:
		return AnytimeIdle
	}
	return 0
}

// MinTurn is the least time a timeslice deployment keeps its cards once
// woken before an equal may take them; Turn raises it from the switch time.
func (p Policy) MinTurn() time.Duration { return time.Duration(p.MinServeSec) * time.Second }

// WaitBeforeTurn is how long an equal's oldest request must have waited
// before the deployment gives way to it.
func (p Policy) WaitBeforeTurn() time.Duration {
	if p.TakesTurns() {
		return TurnWait
	}
	return 0
}

// HoldMax is how long the gateway holds a request for the deployment while
// it wakes.
func (p Policy) HoldMax() time.Duration {
	if p.HoldMaxSec <= 0 {
		return DefaultHoldMax
	}
	return time.Duration(p.HoldMaxSec) * time.Second
}

// Drain says how requests still running are treated when the deployment
// gives its cards up: cut off at once (abort), or let run on for limit
// (zero: however long they take, bounded false) after new ones are held.
func (p Policy) Drain() (abort bool, limit time.Duration) {
	switch p.Unfinished {
	case UnfinishedWait:
		return false, time.Duration(p.UnfinishedWaitSec) * time.Second
	case UnfinishedWaitUnbounded:
		return false, 0
	}
	return true, 0
}

// SleepLevelPref is the sleep level preference, auto when none is stated.
func (p Policy) SleepLevelPref() string {
	if p.SleepLevel == "" {
		return LevelAuto
	}
	return p.SleepLevel
}

// Turn is the least time a deployment keeps its cards once woken, given the
// switch time measured for it (zero when none is known yet).
func Turn(p Policy, switchTime time.Duration) time.Duration {
	if t := SwitchMultiple * switchTime; t > p.MinTurn() {
		return t
	}
	return p.MinTurn()
}

// Holder is a deployment awake on cards another one waits for.
type Holder struct {
	Policy Policy
	// AwakeFor is how long it has been awake since its last wake.
	AwakeFor time.Duration
	// IdleFor is how long it has had no request in flight or waiting; zero
	// while it has one.
	IdleFor time.Duration
	// SwitchTime is the measured time to switch it out and back; zero when
	// unknown.
	SwitchTime time.Duration
}

// Waiter is a deployment waiting for cards.
type Waiter struct {
	Policy Policy
	// Waited is how long its oldest request (or its wish to resume) has
	// waited.
	Waited time.Duration
}

// Reason says why a holder may be put to sleep for a waiter.
type Reason string

// Reasons a holder gives way.
const (
	// ReasonIdle: the holder has been idle for its IdleRelease.
	ReasonIdle Reason = "idle"
	// ReasonRank: the waiter ranks higher and the holder is preemptible.
	ReasonRank Reason = "rank"
	// ReasonTurn: equals taking turns, and the holder has had its turn.
	ReasonTurn Reason = "turn"
)

// Yields says whether holder must give its cards to waiter, and why. A
// never holder does not.
func Yields(holder Holder, waiter Waiter) (Reason, bool) {
	hp, wp := holder.Policy, waiter.Policy
	if !ValidYield(hp.Yield) || hp.Yield == Never {
		return "", false
	}
	if release := hp.IdleRelease(); release > 0 && holder.IdleFor >= release {
		return ReasonIdle, true
	}
	if hp.Preemptible() && wp.Rank() > hp.Rank() {
		return ReasonRank, true
	}
	if hp.TakesTurns() && wp.Rank() == hp.Rank() && holder.AwakeFor >= Turn(hp, holder.SwitchTime) && waiter.Waited >= hp.WaitBeforeTurn() {
		return ReasonTurn, true
	}
	return "", false
}

// SharesCardsWith says a deployment of yield policy a may be placed on cards
// a deployment of policy b holds (awake or asleep). Two idle deployments may
// not share: neither would give way to the other while both are busy, and
// each would hold the other's requests for as long as it stays busy. A never
// deployment shares with no one but what it already shares by sleeping (its
// own choice).
func SharesCardsWith(a, b string) bool {
	if a == Never || b == Never {
		return false
	}
	return !(a == Idle && b == Idle)
}
