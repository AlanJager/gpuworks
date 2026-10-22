package planning

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// PDUpdate is a change asked of a running PD deployment: each role's units
// and the gateway fallback may change. What its instances were launched with
// (the mode, a role's TP and PP, the KV load failure policy) may only be
// restated as it is: changing it means new instances, which is a clone.
type PDUpdate struct {
	Mode          string
	Roles         map[string]PDRole
	KVLoadFailure string
	Fallback      *string
}

// PDUpdated is an update applied: the placement the deployment moves to and
// its mode.
type PDUpdated struct {
	Placement *PDPlacement
	Mode      pdmode.Mode
}

// ApplyPDUpdate applies an update to a running deployment's placement and
// checks the result against its mode (ValidateRunningPD). It does not look
// at nodes; the caller checks the result with ValidatePDNodes.
func ApplyPDUpdate(current *PDPlacement, engine string, update PDUpdate) (PDUpdated, error) {
	if current == nil {
		return PDUpdated{}, pdError(messages.ErrorPDNotPD, "deployment is not PD disaggregated", nil)
	}
	if (update.Mode != "" && update.Mode != current.Mode) || (update.KVLoadFailure != "" && update.KVLoadFailure != current.EffectiveKVLoadFailure()) {
		return PDUpdated{}, pdError(messages.ErrorPDImmutable, "mode and kv_load_failure are fixed at launch; clone the deployment to change them", nil)
	}
	next := *current
	next.Roles = make(map[string]PDRole, len(current.Roles))
	for role, shape := range current.Roles {
		next.Roles[role] = shape
	}
	for role, asked := range update.Roles {
		shape := current.Roles[role]
		if asked.TP != 0 && asked.TP != current.TP(role) {
			return PDUpdated{}, pdError(messages.ErrorPDImmutable, fmt.Sprintf("the %s role's TP is fixed at launch", role), map[string]any{"role": role, "tp": current.TP(role)})
		}
		if asked.PP != 0 && asked.PP != max(shape.PP, 1) {
			return PDUpdated{}, pdError(messages.ErrorPDImmutable, fmt.Sprintf("the %s role's PP is fixed at launch", role), map[string]any{"role": role, "pp": max(shape.PP, 1)})
		}
		shape.Units = asked.Units
		next.Roles[role] = shape
	}
	if update.Fallback != nil {
		next.Fallback = *update.Fallback
	}
	mode, err := ValidateRunningPD(&next, engine)
	if err != nil {
		return PDUpdated{}, err
	}
	return PDUpdated{Placement: &next, Mode: mode}, nil
}

// WithPD is a stored placement with its PD section replaced, every other key
// (node labels) kept as stored.
func WithPD(stored string, pd *PDPlacement) (string, error) {
	document := map[string]any{}
	if stored != "" && stored != "null" {
		if err := json.Unmarshal([]byte(stored), &document); err != nil {
			return "", fmt.Errorf("decode placement: %w", err)
		}
	}
	if document == nil {
		document = map[string]any{}
	}
	document["pd"] = pd
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
