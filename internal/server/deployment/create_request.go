package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// checkRequest applies the rules the request alone answers, places automatic
// units and picks the node of an automatic draft.
func (c *createState) checkRequest() error {
	var err error
	if err := validateNames(c.req.Name, c.req.ServedName); err != nil {
		return invalid(err)
	}
	if strings.TrimSpace(c.req.TargetVendor) != "" && accelerator.NormalizeVendor(c.req.TargetVendor) == "" {
		return invalid(productError(messages.ErrorTargetVendorInvalid, "target_vendor is not supported", map[string]any{"vendor": c.req.TargetVendor}))
	}
	c.unitsAuto, err = c.s.autoPlaceRequest(c.req)
	if err != nil {
		return unitPlacementKind(err)
	}
	defaultUnitStrategy(c.req)
	if err := ValidateReplicaShape(c.req); err != nil {
		return invalid(err)
	}
	if err := ValidateStartTimeout(c.req); err != nil {
		return invalid(err)
	}
	if err := ValidateHealthCheckThreshold(c.req); err != nil {
		return invalid(err)
	}
	if err := c.s.ValidateMountModels(c.req); err != nil {
		return invalid(err)
	}
	if err := checkManualCommandRanks(c.req); err != nil {
		return invalid(err)
	}
	c.pdReq, err = c.s.preparePDDeployRequest(c.req, "")
	if err == nil && c.pdReq != nil {
		_, _, err = c.s.pdGroup(c.pdReq)
	}
	if err != nil {
		return invalid(err)
	}
	if err := c.s.pickAutomaticNode(c.ctx, c.req); err != nil {
		return err
	}
	return nil
}

// resolveTemplate settles the requirements, the template selection and the
// engine.
func (c *createState) resolveTemplate() error {
	var err error
	c.reqs, c.strategy, err = c.s.RequestPlacement(c.req)
	if err != nil {
		return err
	}
	c.profileNode, _ = c.s.d.Store.NodeWithGPUs(templateTargetNodeUUID(c.req.NodeUUID, c.req.NodeUUIDs))
	c.selection, err = c.s.applyTemplateDefaults(c.req, nodeTemplateHardware(c.profileNode))
	if err == nil {
		err = checkPDSpec(c.selection, placementPD(c.req))
	}
	if err == nil && c.selection.ManualCommandDefault {
		// The spec's own command was checked when it was imported, against
		// the layout it was written for; this request may span more nodes.
		err = checkManualCommandRanks(c.req)
	}
	if err != nil {
		return invalid(err)
	}
	c.eng, err = deploy.SelectEngine(c.req.Engine)
	if err == nil {
		err = c.pdReq.checkEngine(c.eng.Name())
	}
	if err != nil {
		return invalid(err)
	}
	preserveEngineModelName(c.req)
	return nil
}
