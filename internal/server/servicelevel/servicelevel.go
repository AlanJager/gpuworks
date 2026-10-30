// Package servicelevel is how a deployment shares GPUs with others: its yield
// policy and the parameters that go with it. It is a leaf, read by the store
// (the columns' values), the gateway (how long a request may wait for a
// wake) and the residency arbiter (who may put whom to sleep).
//
// The rule every policy follows (docs/explanation/service-levels.md): a
// deployment waiting for its cards may have the one awake on them put to
// sleep when that one yields at all and either has been idle for its idle
// time, ranks lower and yields when busy, or takes turns with it and has
// served its turn.
package servicelevel

import "time"

// Yield policies, stored in Deployment.YieldPolicy, in rank order: a
// deployment waiting for cards may take them from a busy one of lower rank
// that yields when busy, and from any yielding one idle for its IdleSec.
const (
	// Never gives its cards to no other deployment by itself; only an
	// operator's or a schedule's sleep frees them. Every deployment from
	// before yield policies is never.
	Never = "never"
	// Idle gives its cards up only once idle for IdleSec, to whoever waits
	// for them; it takes them from timeslice and anytime deployments as soon
	// as a request comes.
	Idle = "idle"
	// TimeSlice also gives its cards up when busy: to a higher-ranked waiter
	// at once, and to another timeslice deployment once it has served
	// MaxServeSec.
	TimeSlice = "timeslice"
	// Anytime gives its cards up to every higher-ranked waiter at once, busy
	// or not, and resumes when the cards are free again.
	Anytime = "anytime"
)

// Policies are the yield policies, highest rank first.
var Policies = []string{Never, Idle, TimeSlice, Anytime}

// ValidYield says p is one of the yield policies.
func ValidYield(p string) bool {
	switch p {
	case Never, Idle, TimeSlice, Anytime:
		return true
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
// cards up while busy (Unfinished).
const (
	// UnfinishedAbort cuts them off at once.
	UnfinishedAbort = "abort"
	// UnfinishedWait lets them run on for UnfinishedWaitSec, then cuts off
	// what is left.
	UnfinishedWait = "wait"
)

// ValidUnfinished says u is one of the unfinished-request handlings.
func ValidUnfinished(u string) bool { return u == UnfinishedAbort || u == UnfinishedWait }

// Bounds of the parameters, in seconds.
const (
	IdleSecMin           = 15
	IdleSecMax           = 7 * 24 * 3600
	MaxServeSecMin       = 30
	MaxServeSecMax       = 24 * 3600
	UnfinishedWaitSecMin = 1
	UnfinishedWaitSecMax = 600
	HoldMaxSecMin        = 10
	HoldMaxSecMax        = 3600
)

// Bound is the range a numeric parameter may take, in seconds. AtMost names
// another parameter it may not exceed when both apply.
type Bound struct {
	Min, Max int
	AtMost   string
}

// Bounds are the ranges of the numeric parameters, by their request names;
// Validate holds a policy to them.
var Bounds = map[string]Bound{
	"idle_sec":            {Min: IdleSecMin, Max: IdleSecMax, AtMost: "max_serve_sec"},
	"max_serve_sec":       {Min: MaxServeSecMin, Max: MaxServeSecMax},
	"unfinished_wait_sec": {Min: UnfinishedWaitSecMin, Max: UnfinishedWaitSecMax},
	"hold_max_sec":        {Min: HoldMaxSecMin, Max: HoldMaxSecMax},
}

// SleepLevels and UnfinishedModes are the values those parameters take.
var (
	SleepLevels     = []string{LevelAuto, LevelL1, LevelL2}
	UnfinishedModes = []string{UnfinishedAbort, UnfinishedWait}
)

// DefaultHoldMax is how long the gateway holds a request for a deployment
// whose policy states no hold time.
const DefaultHoldMax = 2 * time.Minute

// Policy is a deployment's yield policy and its parameters. A parameter that
// does not apply to the policy is its zero value (Normalize); which apply is
// in Applies.
type Policy struct {
	// Yield is one of Never, Idle, TimeSlice, Anytime.
	Yield string
	// IdleSec is how long without a request counts as idle: once idle that
	// long, a deployment of any policy but never gives its cards to whoever
	// waits for them.
	IdleSec int
	// SleepLevel is the level of every sleep the platform chooses for the
	// deployment (a yield): LevelAuto, LevelL1 or LevelL2.
	SleepLevel string
	// MaxServeSec is how long a timeslice deployment keeps its cards once
	// woken while another timeslice deployment waits for them.
	MaxServeSec int
	// Unfinished is what becomes of the requests still running when a
	// timeslice or anytime deployment gives its cards up while busy;
	// UnfinishedWaitSec is how long they may run on for UnfinishedWait.
	Unfinished        string
	UnfinishedWaitSec int
	// HoldMaxSec is how long the gateway holds a request for the deployment
	// while it wakes.
	HoldMaxSec int
}

// Which parameters apply to a policy.
func (p Policy) yields() bool         { return p.Yield != Never }
func (p Policy) usesMaxServe() bool   { return p.Yield == TimeSlice }
func (p Policy) usesUnfinished() bool { return p.Yield == TimeSlice || p.Yield == Anytime }

// Applicable says which of the optional parameters apply, so a client shows
// only those: IdleSec, SleepLevel, MaxServeSec, Unfinished (and
// UnfinishedWaitSec when it is UnfinishedWait) and HoldMaxSec.
type Applicable struct {
	IdleSec, SleepLevel, MaxServeSec, Unfinished, UnfinishedWaitSec, HoldMaxSec bool
}

// Applies is which parameters apply to p.
func (p Policy) Applies() Applicable {
	return Applicable{
		IdleSec: p.yields(), SleepLevel: p.yields(), MaxServeSec: p.usesMaxServe(),
		Unfinished: p.usesUnfinished(), UnfinishedWaitSec: p.usesUnfinished() && p.Unfinished == UnfinishedWait,
		HoldMaxSec: p.yields(),
	}
}

// Normalize is p with every parameter that does not apply cleared, and a
// sleep level that applies but is empty read as auto. A policy read from
// storage or built from a request is normalized, so equal policies compare
// equal.
func (p Policy) Normalize() Policy {
	if p.Yield == "" {
		p.Yield = Never
	}
	if !p.yields() {
		p.IdleSec, p.SleepLevel, p.HoldMaxSec = 0, "", 0
	} else if p.SleepLevel == "" {
		p.SleepLevel = LevelAuto
	}
	if !p.usesMaxServe() {
		p.MaxServeSec = 0
	}
	if !p.usesUnfinished() {
		p.Unfinished = ""
	}
	if p.Unfinished != UnfinishedWait {
		p.UnfinishedWaitSec = 0
	}
	return p
}

// Defaults is the full set of parameters a client that names only a policy
// (and some parameters) gets for the rest. Nothing here is normalized, so the
// wait time is there for a client that turns waiting on.
func Defaults(yield string) Policy {
	p := Policy{Yield: yield, SleepLevel: LevelAuto, UnfinishedWaitSec: 30}
	switch yield {
	case Idle:
		p.IdleSec, p.HoldMaxSec = 60, 300
	case TimeSlice:
		p.IdleSec, p.MaxServeSec, p.Unfinished, p.HoldMaxSec = 30, 120, UnfinishedWait, 600
	case Anytime:
		p.IdleSec, p.Unfinished, p.HoldMaxSec = 30, UnfinishedAbort, 3600
	}
	return p
}

// Overrides are the parameters a request states; nil/empty is not stated and
// takes the policy's default (Build).
type Overrides struct {
	IdleSec           *int
	SleepLevel        string
	MaxServeSec       *int
	Unfinished        string
	UnfinishedWaitSec *int
	HoldMaxSec        *int
}

// Build is the policy a request states: yield with the defaults of that
// policy under the stated parameters, normalized. It does not validate.
func Build(yield string, o Overrides) Policy {
	p := Defaults(yield)
	if o.IdleSec != nil {
		p.IdleSec = *o.IdleSec
	}
	if o.SleepLevel != "" {
		p.SleepLevel = o.SleepLevel
	}
	if o.MaxServeSec != nil {
		p.MaxServeSec = *o.MaxServeSec
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
	if apply.MaxServeSec && (p.MaxServeSec < MaxServeSecMin || p.MaxServeSec > MaxServeSecMax) {
		return &Violation{Field: "max_serve_sec", Reason: ReasonRange, Min: MaxServeSecMin, Max: MaxServeSecMax}
	}
	// Idle that long never comes before the turn ends: the idle time would
	// do nothing.
	if apply.MaxServeSec && p.IdleSec > p.MaxServeSec {
		return &Violation{Field: "idle_sec", Reason: ReasonRange, Min: IdleSecMin, Max: p.MaxServeSec}
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
	if !p.yields() {
		return 0
	}
	return time.Duration(p.IdleSec) * time.Second
}

// Turn is how long a timeslice deployment keeps its cards once woken while
// an equal waits for them.
func (p Policy) Turn() time.Duration { return time.Duration(p.MaxServeSec) * time.Second }

// HoldMax is how long the gateway holds a request for the deployment while
// it wakes.
func (p Policy) HoldMax() time.Duration {
	if p.HoldMaxSec <= 0 {
		return DefaultHoldMax
	}
	return time.Duration(p.HoldMaxSec) * time.Second
}

// Drain says how requests still running are treated when the deployment
// gives its cards up: cut off at once (abort), or let run on for limit after
// new ones are held.
func (p Policy) Drain() (abort bool, limit time.Duration) {
	if p.Unfinished == UnfinishedWait {
		return false, time.Duration(p.UnfinishedWaitSec) * time.Second
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

// Holder is a deployment awake on cards another one waits for.
type Holder struct {
	Policy Policy
	// AwakeFor is how long it has been awake since its last wake.
	AwakeFor time.Duration
	// Busy says it has a request in flight or waiting.
	Busy bool
	// IdleFor is how long it has had no request in flight or waiting; zero
	// while Busy.
	IdleFor time.Duration
}

// Waiter is a deployment waiting for cards.
type Waiter struct {
	Policy Policy
}

// Reason says why a holder may be put to sleep for a waiter.
type Reason string

// Reasons a holder gives way.
const (
	// ReasonIdle: the holder has been idle for its IdleSec.
	ReasonIdle Reason = "idle"
	// ReasonRank: the waiter ranks higher and the holder is preemptible.
	ReasonRank Reason = "rank"
	// ReasonTurn: equals taking turns, and the holder has served its
	// MaxServeSec.
	ReasonTurn Reason = "turn"
)

// Yields says whether holder must give its cards to waiter, and why: by
// TermsOf, against what the holder is doing. A never holder does not.
func Yields(holder Holder, waiter Waiter) (Reason, bool) {
	terms := TermsOf(holder.Policy, waiter.Policy)
	switch {
	case terms.Never:
		return "", false
	case !holder.Busy && holder.IdleFor >= holder.Policy.IdleRelease():
		return ReasonIdle, true
	case terms.AtOnce:
		return ReasonRank, true
	case terms.Turn > 0 && holder.AwakeFor >= terms.Turn:
		return ReasonTurn, true
	}
	return "", false
}

// Due is how long until holder must give its cards to waiter if nothing
// else changes: the rest of its idle time, or of its turn. ok is false when
// no wait ends it - a never holder, or a busy one that yields to the waiter
// only once idle (when its requests end, which no clock tells). Zero when it
// must already (Yields).
func Due(holder Holder, waiter Waiter) (time.Duration, bool) {
	if _, ok := Yields(holder, waiter); ok {
		return 0, true
	}
	terms := TermsOf(holder.Policy, waiter.Policy)
	if terms.Never {
		return 0, false
	}
	var due time.Duration
	ok := false
	if !holder.Busy {
		due, ok = terms.Idle-holder.IdleFor, true
	}
	if terms.Turn > 0 {
		if left := terms.Turn - holder.AwakeFor; !ok || left < due {
			due, ok = left, true
		}
	}
	return due, ok
}

// Terms are when a holder gives its cards to a waiter: AtOnce, whatever it
// is doing; else once idle for Idle, or, when Turn is set, once it has served
// that long. Never is a holder that does not give them up at all.
type Terms struct {
	Never  bool
	AtOnce bool
	Idle   time.Duration
	Turn   time.Duration
}

// TermsOf are the terms of Yields, stated without a holder's state: what a
// waiter is told it waits for.
func TermsOf(holder, waiter Policy) Terms {
	if !ValidYield(holder.Yield) || holder.Yield == Never {
		return Terms{Never: true}
	}
	if holder.Preemptible() && waiter.Rank() > holder.Rank() {
		return Terms{AtOnce: true}
	}
	out := Terms{Idle: holder.IdleRelease()}
	if holder.TakesTurns() && waiter.Rank() == holder.Rank() {
		out.Turn = holder.Turn()
	}
	return out
}

// SharesCardsWith says a deployment of yield policy a may be placed on cards
// a deployment of policy b holds (awake or asleep). A never deployment
// shares with no one but what it already shares by sleeping (its own
// choice). Two idle deployments may share, though neither gives way to the
// other while both are busy (IdleNeighbours warns of it).
func SharesCardsWith(a, b string) bool { return a != Never && b != Never }

// IdleNeighbours says two deployments of these policies on the same cards
// each hold the other's requests for as long as it stays busy: both give
// their cards up only when idle.
func IdleNeighbours(a, b string) bool { return a == Idle && b == Idle }
