package inferencecatalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/pdmode"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
)

// MaxManualCommandBytes bounds a spec's manual command, as it does a
// pre-start script.
const MaxManualCommandBytes = 64 << 10

// PDRoleShape is one role of the layout a PD-only spec is written for: the
// degrees one instance of the role runs at, how many nodes it spans, and how
// many instances a deployment starts with. The deploy form starts from it; a
// deployment may still change the instance count and pick its nodes.
type PDRoleShape struct {
	TP int `yaml:"tp" json:"tp"`
	// PP is the pipeline degree; zero means one.
	PP int `yaml:"pp,omitempty" json:"pp,omitempty"`
	// Nodes is how many nodes one instance spans (its leader and followers);
	// zero means one. TP x PP is divided evenly over them.
	Nodes int `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	// Units is how many instances of the role a deployment starts with;
	// zero means one.
	Units int `yaml:"units,omitempty" json:"units,omitempty"`
}

// NodeCount is the nodes one instance spans.
func (s PDRoleShape) NodeCount() int { return max(s.Nodes, 1) }

// UnitCount is the instances a deployment starts with.
func (s PDRoleShape) UnitCount() int { return max(s.Units, 1) }

// PipelineDegree is the pipeline degree, one when unstated.
func (s PDRoleShape) PipelineDegree() int { return max(s.PP, 1) }

// GPUsPerNode is the cards one instance takes on each of its nodes.
func (s PDRoleShape) GPUsPerNode() int { return s.TP * s.PipelineDegree() / s.NodeCount() }

// PDRoleShapes is a PD-only spec's layout by role name
// (inference_specs.pd_roles).
type PDRoleShapes map[string]PDRoleShape

// EncodePDRoleShapes encodes the inference_specs.pd_roles column; a spec
// without a layout stores "".
func EncodePDRoleShapes(shapes PDRoleShapes) string {
	if len(shapes) == 0 {
		return ""
	}
	raw, _ := json.Marshal(shapes)
	return string(raw)
}

// DecodePDRoleShapes decodes the inference_specs.pd_roles column; a blank
// column states no layout.
func DecodePDRoleShapes(raw string) (PDRoleShapes, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var shapes PDRoleShapes
	if err := json.Unmarshal([]byte(raw), &shapes); err != nil {
		return nil, err
	}
	return shapes, nil
}

// validatePDShape checks a spec's PD layout and manual command against its PD
// mode: a layout names every role of the mode and nothing else, with degrees
// the mode's pairing rules take and nodes that divide them; a manual command
// of a PD spec tells its roles (and the ranks of a multi-node role) apart, as
// a deployment's own manual command must (deploy.ManualCommandRankBlind).
func validatePDShape(modelID string, spec *InferenceSpec) error {
	spec.ManualCommand = strings.TrimSpace(spec.ManualCommand)
	if len(spec.ManualCommand) > MaxManualCommandBytes {
		return fmt.Errorf("model %s spec %s manual_command is longer than %d bytes", modelID, spec.Name, MaxManualCommandBytes)
	}
	if strings.HasPrefix(spec.ManualCommand, "[") {
		return fmt.Errorf("model %s spec %s manual_command is a shell script, not a JSON argv", modelID, spec.Name)
	}
	if len(spec.PDRoles) == 0 {
		if spec.PDMode != "" && spec.ManualCommand != "" && !mentionsVariable(spec.ManualCommand, placementeval.VarPDRole) {
			return fmt.Errorf("model %s spec %s manual_command runs on both PD roles and must reference ${%s%s}", modelID, spec.Name, placementeval.VariablePrefix, placementeval.VarPDRole)
		}
		return nil
	}
	if spec.PDMode == "" {
		return fmt.Errorf("model %s spec %s states pd_roles without a pd_mode", modelID, spec.Name)
	}
	mode, _ := pdmode.Lookup(spec.PDMode)
	multiNode := false
	for role, shape := range spec.PDRoles {
		if !mode.HasRole(role) {
			return fmt.Errorf("model %s spec %s pd_roles names %q, which pd_mode %s has no role for (roles: %s)", modelID, spec.Name, role, mode.Name, strings.Join(mode.Roles, ", "))
		}
		switch {
		case shape.TP < 1:
			return fmt.Errorf("model %s spec %s pd_roles.%s needs tp of at least 1", modelID, spec.Name, role)
		case shape.PP < 0 || shape.Nodes < 0 || shape.Units < 0:
			return fmt.Errorf("model %s spec %s pd_roles.%s cannot state a negative pp, nodes or units", modelID, spec.Name, role)
		case shape.UnitCount() > pdmode.MaxUnitsPerRole:
			return fmt.Errorf("model %s spec %s pd_roles.%s starts more than %d instances", modelID, spec.Name, role, pdmode.MaxUnitsPerRole)
		case shape.TP*shape.PipelineDegree()%shape.NodeCount() != 0:
			return fmt.Errorf("model %s spec %s pd_roles.%s spreads TP %d x PP %d over %d nodes, which does not divide evenly", modelID, spec.Name, role, shape.TP, shape.PipelineDegree(), shape.NodeCount())
		}
		multiNode = multiNode || shape.NodeCount() > 1
	}
	for _, role := range mode.Roles {
		if _, ok := spec.PDRoles[role]; !ok {
			return fmt.Errorf("model %s spec %s pd_roles has no %s role", modelID, spec.Name, role)
		}
	}
	prefill, decode := spec.PDRoles[pdmode.RolePrefill], spec.PDRoles[pdmode.RoleDecode]
	if err := mode.CheckTP(prefill.TP, decode.TP); err != nil {
		return fmt.Errorf("model %s spec %s pd_roles: %w", modelID, spec.Name, err)
	}
	if err := mode.CheckPP(prefill.PipelineDegree(), decode.PipelineDegree()); err != nil {
		return fmt.Errorf("model %s spec %s pd_roles: %w", modelID, spec.Name, err)
	}
	if spec.ManualCommand == "" {
		return nil
	}
	if !mentionsVariable(spec.ManualCommand, placementeval.VarPDRole) {
		return fmt.Errorf("model %s spec %s manual_command runs on both PD roles and must reference ${%s%s}", modelID, spec.Name, placementeval.VariablePrefix, placementeval.VarPDRole)
	}
	if multiNode && !mentionsVariable(spec.ManualCommand, placementeval.VarNodeRank) && !mentionsVariable(spec.ManualCommand, placementeval.VarHeadless) {
		return fmt.Errorf("model %s spec %s manual_command runs on every rank of a multi-node role and must reference ${%s%s}", modelID, spec.Name, placementeval.VariablePrefix, placementeval.VarNodeRank)
	}
	return nil
}

func mentionsVariable(script, name string) bool {
	return slices.Contains(placementeval.References(script), placementeval.VariablePrefix+name)
}
