package planning

import (
	"errors"
	"fmt"
	"slices"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// What a deployment's schedule may order: sleep and wake where the
// deployment can sleep (SleepEligibility), and a replica count - for a PD
// deployment, one per role - within ReplicaBounds where
// ScheduledReplicasRefusal allows it. The API's schedule validation, the
// automation's scaling and the scheduling views read them from here.

// ReplicaBounds are the fewest and most replicas an order of the deployment
// may set: 1 to MaxReplicas, and for a PD role 1 to pdmode.MaxUnitsPerRole -
// or 0 where the mode lets the role be empty (prefill: without it every
// request is decoded alone).
func ReplicaBounds(dep *store.Deployment, role string) (lowest, highest int) {
	pd, _ := PDOf(dep.Placement)
	if pd == nil {
		return 1, MaxReplicas
	}
	if mode, ok := pdmode.Lookup(pd.Mode); ok && mode.MayBeEmpty(role) {
		return 0, pdmode.MaxUnitsPerRole
	}
	return 1, pdmode.MaxUnitsPerRole
}

// ScheduledReplicasRefusal says why a schedule cannot set a deployment's
// replica count, or nil when it can. A single-node deployment, and a
// multi-node or PD deployment the platform places (units_auto), can: new
// replicas get their nodes the way an operator's scaling picks them
// (Scaling). A deployment whose replicas the operator placed by hand names
// the nodes of every replica, which a count cannot say.
//
// A PD deployment scales each role with its own count, so its order names the
// role (one of its mode's); any other deployment's order names none.
func ScheduledReplicasRefusal(dep *store.Deployment, role string) error {
	pd, _ := PDOf(dep.Placement)
	if pd == nil {
		if role != "" {
			return scheduleRoleInvalid("only a PD deployment's order names a role", nil)
		}
	} else {
		mode, ok := pdmode.Lookup(pd.Mode)
		switch {
		case role == "":
			return scheduleRoleInvalid("a PD deployment's order names the role it scales", map[string]any{"roles": slices.Clone(mode.Roles)})
		case ok && !mode.HasRole(role):
			return scheduleRoleInvalid(fmt.Sprintf("the %s mode has no %q role", pd.Mode, role), map[string]any{"roles": slices.Clone(mode.Roles)})
		}
	}
	if (dep.NNodes > 1 || pd != nil) && !dep.UnitsAuto {
		return messages.NewProductError(messages.ErrorScheduleReplicasManualPlacement, "the deployment's replicas are placed by hand; switch it to automatic placement to schedule its replica count", nil)
	}
	return nil
}

func scheduleRoleInvalid(reason string, extra map[string]any) error {
	params := map[string]any{"field": "role", "reason": reason}
	for k, v := range extra {
		params[k] = v
	}
	return messages.NewProductError(messages.ErrorScheduleInvalid, "schedule role: "+reason, params)
}

// ReplicasOutOfBounds refuses a PD role's replica count outside the role's
// bounds, with the code the PD update gives a role it cannot leave empty or
// over-fill.
func ReplicasOutOfBounds(role string, replicas, lowest, highest int) error {
	if replicas < lowest {
		return messages.NewProductError(messages.ErrorPDRoleUnitsEmpty, fmt.Sprintf("the %s role needs at least one instance", role), map[string]any{"role": role})
	}
	return messages.NewProductError(messages.ErrorPDRoleUnitsTooMany, fmt.Sprintf("the %s role has more than %d instances", role, highest), map[string]any{"role": role, "maximum": highest})
}

// ScheduleCapabilities is what a deployment's schedule may order. A refusal
// is a stable product code, "" when the order is allowed.
type ScheduleCapabilities struct {
	// Sleep and Wake refuse a sleep or wake order (SleepEligibility).
	Sleep, Wake string
	// Replicas are the replica counts it may set: one entry with no role for
	// any deployment but PD, one per role of its mode, in the mode's order,
	// for a PD deployment.
	Replicas []ReplicaCapability
}

// ReplicaCapability is the replica count a schedule may set for a
// deployment, or one PD role of it.
type ReplicaCapability struct {
	Role            string
	Lowest, Highest int
	// Refusal refuses any replicas order (ScheduledReplicasRefusal), ""
	// when allowed.
	Refusal string
}

// ScheduleCapabilitiesOf is what dep's schedule may order.
func ScheduleCapabilitiesOf(dep *store.Deployment) ScheduleCapabilities {
	code := SleepEligibility(dep)
	out := ScheduleCapabilities{Sleep: code, Wake: code}
	roles := []string{""}
	if pd, _ := PDOf(dep.Placement); pd != nil {
		roles = nil
		if mode, ok := pdmode.Lookup(pd.Mode); ok {
			roles = slices.Clone(mode.Roles)
		}
	}
	for _, role := range roles {
		lowest, highest := ReplicaBounds(dep, role)
		capability := ReplicaCapability{Role: role, Lowest: lowest, Highest: highest}
		if err := ScheduledReplicasRefusal(dep, role); err != nil {
			capability.Refusal = refusalCode(err)
		}
		out.Replicas = append(out.Replicas, capability)
	}
	return out
}

// refusalCode is a product error's code; any other error is ErrorScheduleInvalid.
func refusalCode(err error) string {
	var product *messages.ProductError
	if errors.As(err, &product) {
		return product.Code()
	}
	return messages.ErrorScheduleInvalid
}
