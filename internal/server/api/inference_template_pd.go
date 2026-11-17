package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// pdRoleShapesView is a spec's PD layout with every default stated: a stored
// layout that does not decode states none.
func pdRoleShapesView(raw string) map[string]dto.InferencePDRoleShape {
	shapes, err := inferencecatalog.DecodePDRoleShapes(raw)
	if err != nil || len(shapes) == 0 {
		return nil
	}
	out := make(map[string]dto.InferencePDRoleShape, len(shapes))
	for role, shape := range shapes {
		out[role] = dto.InferencePDRoleShape{TP: shape.TP, PP: shape.PipelineDegree(), Nodes: shape.NodeCount(), GPUsPerNode: shape.GPUsPerNode(), Units: shape.UnitCount()}
	}
	return out
}
