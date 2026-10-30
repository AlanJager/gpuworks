package dto

import "gpuworks.ai/gpuworks/internal/server/servicelevel"

// YieldPolicy is a deployment's yield policy and the parameters that apply
// to it (docs/explanation/service-levels.md): the body PUT
// /deployments/{id}/yield-policy answers and the deployment's yield_policy.
// A parameter that does not apply to the policy is absent.
//
// IdleNeighbours, set only in the answer to PUT, names the other idle-policy
// deployments on the deployment's cards when it is idle-policy too: each of
// them holds the other's requests for as long as it stays busy.
type YieldPolicy struct {
	Policy            string   `json:"policy"`
	IdleSec           *int     `json:"idle_sec,omitempty"`
	SleepLevel        string   `json:"sleep_level,omitempty"`
	MaxServeSec       *int     `json:"max_serve_sec,omitempty"`
	Unfinished        string   `json:"unfinished,omitempty"`
	UnfinishedWaitSec *int     `json:"unfinished_wait_sec,omitempty"`
	HoldMaxSec        *int     `json:"hold_max_sec,omitempty"`
	IdleNeighbours    []string `json:"idle_neighbours,omitempty"`
}

// YieldPolicyOf is the wire form of a policy.
func YieldPolicyOf(p servicelevel.Policy) YieldPolicy {
	p = p.Normalize()
	apply := p.Applies()
	out := YieldPolicy{Policy: p.Yield}
	num := func(applies bool, v int) *int {
		if !applies {
			return nil
		}
		return &v
	}
	out.IdleSec = num(apply.IdleSec, p.IdleSec)
	out.MaxServeSec = num(apply.MaxServeSec, p.MaxServeSec)
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
// /deployments/{id}/yield-policy and as a create request's yield_policy: a
// policy with the parameters to set. A parameter left out takes the policy's
// default and one that does not apply to the policy is ignored, as is
// idle_neighbours, so a response can be sent back as it is.
type YieldPolicyRequest struct {
	Policy            string   `json:"policy"`
	IdleSec           *int     `json:"idle_sec,omitempty"`
	SleepLevel        string   `json:"sleep_level,omitempty"`
	MaxServeSec       *int     `json:"max_serve_sec,omitempty"`
	Unfinished        string   `json:"unfinished,omitempty"`
	UnfinishedWaitSec *int     `json:"unfinished_wait_sec,omitempty"`
	HoldMaxSec        *int     `json:"hold_max_sec,omitempty"`
	IdleNeighbours    []string `json:"idle_neighbours,omitempty"`
}

// Parse is the policy the request states, or the violation that refuses it.
// A request without a policy is refused as a policy that is required.
func (r YieldPolicyRequest) Parse() (servicelevel.Policy, *servicelevel.Violation) {
	if r.Policy == "" {
		return servicelevel.Policy{}, &servicelevel.Violation{Field: "policy", Reason: servicelevel.ReasonRequired}
	}
	return servicelevel.Parse(r.Policy, servicelevel.Overrides{
		IdleSec: r.IdleSec, SleepLevel: r.SleepLevel, MaxServeSec: r.MaxServeSec,
		Unfinished: r.Unfinished, UnfinishedWaitSec: r.UnfinishedWaitSec, HoldMaxSec: r.HoldMaxSec,
	})
}

// YieldPolicyDictionary is GET /api/v1/scheduling/yield-policies: what a
// client needs to edit a yield policy, all of it from the server's rules
// (package servicelevel), so no client keeps its own copy.
//
// Policies are in rank order, highest first. Each one's Defaults are what
// choosing it fills in, including unfinished_wait_sec wherever unfinished
// applies, for a client that turns waiting on; Applies names the parameters
// that apply to it, unfinished_wait_sec only while unfinished is "wait".
// Bounds are the numeric parameters' ranges in seconds; at_most names a
// parameter one may not exceed when both apply.
type YieldPolicyDictionary struct {
	Policies    []YieldPolicyEntry    `json:"policies"`
	Bounds      map[string]YieldBound `json:"bounds"`
	SleepLevels []string              `json:"sleep_levels"`
	Unfinished  []string              `json:"unfinished"`
}

// YieldPolicyEntry is one policy of the dictionary.
type YieldPolicyEntry struct {
	Policy   string      `json:"policy"`
	Defaults YieldPolicy `json:"defaults"`
	Applies  []string    `json:"applies"`
}

// YieldBound is a numeric parameter's range.
type YieldBound struct {
	Minimum int    `json:"minimum"`
	Maximum int    `json:"maximum"`
	AtMost  string `json:"at_most,omitempty"`
}

// YieldPolicyDictionaryOf is the dictionary of the server's rules.
func YieldPolicyDictionaryOf() YieldPolicyDictionary {
	out := YieldPolicyDictionary{
		Policies: []YieldPolicyEntry{}, Bounds: map[string]YieldBound{},
		SleepLevels: servicelevel.SleepLevels, Unfinished: servicelevel.UnfinishedModes,
	}
	for _, name := range servicelevel.Policies {
		defaults := servicelevel.Defaults(name)
		entry := YieldPolicyEntry{Policy: name, Defaults: YieldPolicyOf(defaults), Applies: []string{}}
		apply := defaults.Normalize().Applies()
		if apply.Unfinished && entry.Defaults.UnfinishedWaitSec == nil {
			wait := defaults.UnfinishedWaitSec
			entry.Defaults.UnfinishedWaitSec = &wait
		}
		for _, p := range []struct {
			name    string
			applies bool
		}{
			{"idle_sec", apply.IdleSec}, {"sleep_level", apply.SleepLevel}, {"max_serve_sec", apply.MaxServeSec},
			{"unfinished", apply.Unfinished}, {"unfinished_wait_sec", apply.Unfinished}, {"hold_max_sec", apply.HoldMaxSec},
		} {
			if p.applies {
				entry.Applies = append(entry.Applies, p.name)
			}
		}
		out.Policies = append(out.Policies, entry)
	}
	for name, b := range servicelevel.Bounds {
		out.Bounds[name] = YieldBound{Minimum: b.Min, Maximum: b.Max, AtMost: b.AtMost}
	}
	return out
}
