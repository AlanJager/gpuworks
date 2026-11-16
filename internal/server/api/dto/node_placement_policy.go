package dto

import (
	"encoding/json"
	"errors"

	"gpuworks.ai/gpuworks/internal/server/nodepolicy"
)

// NodePlacementPolicySetting is one entry of GET
// /api/v1/scheduling/node-placement-policies: a knob of the node placement
// policy, which the UI renders by kind and labels by key
// (nodes:placementPolicy.<key>.*), so a new knob needs no client change.
type NodePlacementPolicySetting struct {
	Key     string `json:"key"`
	Kind    string `json:"kind"`
	Default any    `json:"default"`
}

// NodePlacementPolicyDictionaryOf is the dictionary, from nodepolicy.Settings.
func NodePlacementPolicyDictionaryOf() []NodePlacementPolicySetting {
	settings := nodepolicy.Settings()
	out := make([]NodePlacementPolicySetting, 0, len(settings))
	for _, setting := range settings {
		out = append(out, NodePlacementPolicySetting{Key: string(setting.Key), Kind: string(setting.Kind), Default: setting.Default})
	}
	return out
}

// NodePlacementPolicyAvailability says whether one knob takes effect on the
// node; Reason (a messages.NodePlacementPolicy* code) says why not. A knob
// that does not apply may still be set.
type NodePlacementPolicyAvailability struct {
	Key        string `json:"key"`
	Applicable bool   `json:"applicable"`
	Reason     string `json:"reason,omitempty"`
}

// NodePlacementPolicyOf is a node's policy as Node.PlacementPolicy shows it,
// keyed as the dictionary is, and each knob's availability from the facts
// the caller read off the node row: a ready partition backend, and an agent
// that keeps processes outside the platform reported.
func NodePlacementPolicyOf(policy nodepolicy.Policy, partitionBackendReady, tracksForeign bool) (map[string]any, []NodePlacementPolicyAvailability) {
	values := nodepolicy.Values(policy)
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[string(key)] = value
	}
	facts := nodepolicy.Facts{PartitionBackendReady: partitionBackendReady, TracksForeign: tracksForeign}
	var availability []NodePlacementPolicyAvailability
	for _, item := range nodepolicy.Applicability(facts) {
		availability = append(availability, NodePlacementPolicyAvailability{Key: string(item.Key), Applicable: item.Applicable, Reason: item.Reason})
	}
	return out, availability
}

// NodePlacementPolicyPatch is the body of PATCH
// /api/v1/nodes/{uuid}/placement-policy: the knobs to change, by key.
type NodePlacementPolicyPatch map[string]json.RawMessage

// Apply writes the patch into policy through nodepolicy.Apply. A refused key
// comes back as field (the knob's key) for the field error.
func (p NodePlacementPolicyPatch) Apply(policy nodepolicy.Policy) (out nodepolicy.Policy, field string, err error) {
	patch := make(map[nodepolicy.Key]json.RawMessage, len(p))
	for key, raw := range p {
		patch[nodepolicy.Key(key)] = raw
	}
	out, err = nodepolicy.Apply(policy, patch)
	var invalid *nodepolicy.InvalidError
	if errors.As(err, &invalid) {
		field = string(invalid.Key)
	}
	return out, field, err
}
