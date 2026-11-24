package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// resolveTemplate reads the request, applies the template selection and
// starts the response every later exit carries.
func (r *evaluateState) resolveTemplate() bool {
	// An automatic multi-node or PD request is previewed on the nodes the
	// server would pick now; the create picks again.
	autoPlaced, autoErr := r.s.autoPlaceRequest(r.req)
	if autoErr != nil {
		r.errs.add("placement_mode", autoErr)
		r.out.fieldErrors(&dto.EvaluateResponse{SchemaVersion: evaluateSchemaVersion, Replicas: r.req.Replicas, AutoPlaced: true}, r.errs)
		return false
	}
	r.reqs = r.s.collectEvaluationRequestErrors(r.req, r.errs)
	r.profileNode, _ = r.s.d.Store.NodeWithGPUs(templateTargetNodeUUID(r.req.NodeUUID, r.req.NodeUUIDs))
	r.targetVendor = accelerator.NormalizeVendor(r.statedVendor)
	if strings.TrimSpace(r.statedVendor) != "" && r.targetVendor == "" {
		r.errs.add("target_vendor", productError(messages.ErrorTargetVendorInvalid, "target_vendor is not supported", map[string]any{"vendor": r.statedVendor}))
	}
	if r.targetVendor == "" {
		r.targetVendor = planning.RequirementVendor(r.reqs)
	}
	hardware := nodeTemplateHardware(r.profileNode)
	// An explicitly named spec is a complete execution contract and resolves
	// without hardware facts; injecting the vendor hint here would turn the
	// cc-less GPU into a chipmatch rejection and make automatic placement with
	// a selected template impossible. The vendor hint serves the unnamed-spec
	// matching path only.
	if hardware.GPU.Vendor == "" && r.req.TemplateSpecName == "" {
		hardware.GPU.Vendor = r.targetVendor
	}
	var err error
	r.selection, err = r.s.applyTemplateDefaults(r.req, hardware)
	if err != nil {
		r.out.fail(err)
		return false
	}
	r.errs.add(fieldPDPlacement, checkPDSpec(r.selection, placementPD(r.req)))
	preserveEngineModelName(r.req)
	// The mounts the request states are judged against the assembled launch's
	// own rules here, so the form learns about a duplicate or nested target at
	// the same moment it learns everything else about the draft.
	if err := validateMountRowRequest(r.req, mountRowTargets(r.selection.Components, requestModelMount(r.req))); err != nil {
		r.errs.add(FieldContainerMounts, err)
	} else if err := r.s.ValidateMountModels(r.req); err != nil {
		r.errs.add(FieldContainerMounts, err)
	}
	// Every refused docker arg, at once, beside the docker args field.
	// Materialization would refuse only the first, and its errors start on
	// the extra args field.
	ownOverrides, _ := RequestContainerOverrides(r.req.ContainerOverrides)
	for _, err := range deploy.DockerArgErrors(ownOverrides.DockerArgs, ownOverrides.ShmBytes) {
		r.errs.add(FieldContainerDocker, err)
	}
	// Each replica is placed as its own unit; this tree admits no joint
	// multi-replica group, so the scope is always per-replica. The response
	// exists from here on so every field-error exit carries the template facts
	// it already resolved.
	r.resp = dto.EvaluateResponse{SchemaVersion: evaluateSchemaVersion, OK: true, Replicas: r.req.Replicas, AutoPlaced: autoPlaced, PlanScope: "per_replica", TemplateMatch: r.selection.Match, MatchedBy: r.selection.MatchedBy, TemplateDeviated: r.selection.Deviated}
	if r.selection.ManualCommandDefault {
		// The spec owns its command: the editor starts from that script.
		r.resp.ManualCommandDraft = r.selection.Spec.ManualCommand
	}
	if r.selection.Spec != nil {
		r.resp.TemplateSpecSource = r.selection.Spec.Source
		r.resp.TemplateSpecName = r.selection.Spec.Name
	}
	r.resp.Warnings = append(r.resp.Warnings, r.selection.Warnings...)
	// Host-access docker args are allowed; the preview names each one, the
	// template's and the deployment's own, refused args or not.
	r.resp.Warnings = append(r.resp.Warnings, deploy.DockerArgHostAccessWarnings(append(append([]string(nil), r.selection.dockerArgs()...), ownOverrides.DockerArgs...))...)
	// A placement read failure was collected above; the zero placement keeps
	// the rest of the answer renderable.
	r.pool, r.poolErr = requestPool(r.req)
	r.placement = evaluationPlacement(r.req)
	return true
}
