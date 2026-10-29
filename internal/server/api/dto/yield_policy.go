package dto

import "gpuworks.ai/gpuworks/internal/server/servicelevel"

// YieldPolicy is a deployment's yield policy and the parameters that apply
// to it (docs/explanation/service-levels.md): the body PUT
// /deployments/{id}/yield-policy answers and the deployment's yield_policy.
//
// Preset is derived: resident, online, shared or offline when the stored
// parameters are exactly that service level's, else custom. A parameter that
// does not apply to the policy is absent.
type YieldPolicy struct {
	Preset            string `json:"preset"`
	Policy            string `json:"policy"`
	AutoSleep         bool   `json:"auto_sleep"`
	IdleSec           *int   `json:"idle_sec,omitempty"`
	SleepLevel        string `json:"sleep_level,omitempty"`
	MinServeSec       *int   `json:"min_serve_sec,omitempty"`
	Unfinished        string `json:"unfinished,omitempty"`
	UnfinishedWaitSec *int   `json:"unfinished_wait_sec,omitempty"`
	HoldMaxSec        *int   `json:"hold_max_sec,omitempty"`
}

// YieldPolicyOf is the wire form of a policy.
func YieldPolicyOf(p servicelevel.Policy) YieldPolicy {
	p = p.Normalize()
	apply := p.Applies()
	out := YieldPolicy{Preset: servicelevel.PresetOf(p), Policy: p.Yield, AutoSleep: p.AutoSleep}
	num := func(applies bool, v int) *int {
		if !applies {
			return nil
		}
		return &v
	}
	out.IdleSec = num(apply.IdleSec, p.IdleSec)
	out.MinServeSec = num(apply.MinServeSec, p.MinServeSec)
	out.UnfinishedWaitSec = num(apply.UnfinishedWaitSec, p.UnfinishedWaitSec)
	out.HoldMaxSec = num(apply.HoldMaxSec, p.HoldMaxSec)
	if apply.SleepLevel {
		out.SleepLevel = p.SleepLevel
	}
	if apply.Unfinished {
		out.Unfinished = p.Unfinished
	}
	return out
}

// YieldPolicyRequest is the yield policy a request states, in the body of PUT
// /deployments/{id}/yield-policy and as a create request's yield_policy:
// either {"preset": "online"} alone (resident, online, shared or offline: the
// service level's full set of parameters), or a policy with the parameters to
// set; a parameter left out takes the policy's default (the parameters of the
// preset the policy is the policy of) and one that does not apply to the
// policy is ignored. When both preset and policy are given the preset, which
// is derived, is ignored, so a response can be sent back as it is.
type YieldPolicyRequest struct {
	Preset            string `json:"preset,omitempty"`
	Policy            string `json:"policy,omitempty"`
	AutoSleep         *bool  `json:"auto_sleep,omitempty"`
	IdleSec           *int   `json:"idle_sec,omitempty"`
	SleepLevel        string `json:"sleep_level,omitempty"`
	MinServeSec       *int   `json:"min_serve_sec,omitempty"`
	Unfinished        string `json:"unfinished,omitempty"`
	UnfinishedWaitSec *int   `json:"unfinished_wait_sec,omitempty"`
	HoldMaxSec        *int   `json:"hold_max_sec,omitempty"`
}

// Parse is the policy the request states, or the violation that refuses it.
// An empty request, or a preset with parameters but no policy, is refused as
// a policy that is required.
func (r YieldPolicyRequest) Parse() (servicelevel.Policy, *servicelevel.Violation) {
	if r.Policy == "" {
		stated := r.AutoSleep != nil || r.IdleSec != nil || r.SleepLevel != "" || r.MinServeSec != nil ||
			r.Unfinished != "" || r.UnfinishedWaitSec != nil || r.HoldMaxSec != nil
		if r.Preset == "" || stated {
			return servicelevel.Policy{}, &servicelevel.Violation{Field: "policy", Reason: servicelevel.ReasonRequired}
		}
		p, ok := servicelevel.PresetPolicy(r.Preset)
		if !ok {
			return servicelevel.Policy{}, &servicelevel.Violation{Field: "preset", Reason: servicelevel.ReasonValue}
		}
		return p.Normalize(), nil
	}
	return servicelevel.Parse(r.Policy, servicelevel.Overrides{
		AutoSleep: r.AutoSleep, IdleSec: r.IdleSec, SleepLevel: r.SleepLevel, MinServeSec: r.MinServeSec,
		Unfinished: r.Unfinished, UnfinishedWaitSec: r.UnfinishedWaitSec, HoldMaxSec: r.HoldMaxSec,
	})
}
