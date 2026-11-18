package deployment

import (
	"strings"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// resolveEngineAndModel settles the engine, the image default and the model.
// A draft with a field error by now stops here.
func (r *evaluateState) resolveEngineAndModel() bool {
	var err error
	r.eng, err = deploy.SelectEngine(r.req.Engine)
	if err != nil {
		r.errs.add("engine", productError(messages.ErrorEngineInvalid, "unknown engine", map[string]any{"engine": r.req.Engine}))
		r.writeFieldErrors()
		return false
	}
	// Placement keeps the target-specific sleep refusals and their diagnostics.
	if sleepErr := planning.SleepDeclarable(r.req.Sleepable, r.eng.Name(), ""); sleepErr != nil {
		r.errs.add("sleepable", SleepDeclarationError(sleepErr))
	}
	// Whether the form may offer a yield policy other than never: the create's
	// own check, answered even when a field error stops the preview below.
	yieldEligible := dto.EligibilityOf(planning.DraftYieldPolicyRefusal(r.req.Sleepable, r.eng.Name(), r.req.NNodes, placementPD(r.req) != nil))
	r.resp.YieldPolicyEligible = &yieldEligible
	if !r.errs.empty() {
		// A draft with an early field error is what the form must fix before a
		// preview means anything; continuing would only re-report the same
		// rules from the assembled launch.
		r.writeFieldErrors()
		return false
	}
	profile, err := deploy.ProfileForTarget(r.eng.Name(), r.targetVendor)
	if err != nil {
		r.out.fail(err)
		return false
	}
	r.resp.Image = r.req.Image
	if r.resp.Image == "" {
		r.resp.Image = profile.DefaultImage
	}
	r.recommend = r.excludeDeployment == 0 && needsDistributedRecommendation(r.req, r.resp.AutoPlaced)
	r.selection.previewWeights = r.recommend
	r.preflightSelection = r.selection
	r.preflightSelection.Components = append([]deploy.ComponentBinding(nil), r.selection.Components...)
	r.model, err = r.s.resolveDeploymentModelFromTemplate(r.ctx, r.req.ModelID, &r.selection, r.req.TemplateID)
	if r.errs.add("model_id", err) {
		r.writeFieldErrors()
		return false
	}
	r.s.noteHealthCheckableOf(&r.resp, &r.selection, r.model)
	// A quick registration may not have metadata yet. Keep evaluation and
	// scheduling usable, but tell clients that the VRAM estimate is incomplete.
	if r.model.WeightBytes <= 0 && (r.selection.Spec == nil || (r.selection.Spec.WeightBytes <= 0 && r.selection.Spec.RuntimeWeightBytes <= 0)) {
		r.resp.Skipped = true
		r.resp.SkipReason = "model weight size is unknown"
	}
	return true
}

// materializeLaunch decides the launch configuration and answers the form
// values read from it.
func (r *evaluateState) materializeLaunch() bool {
	r.servedName = strings.TrimSpace(r.req.ServedName)
	if strings.Contains(strings.ToLower(strings.TrimSpace(r.req.Image)), "@sha256:") {
		r.resp.Warnings = append(r.resp.Warnings, messages.NewWarning(messages.WarningImageDigestUnverified, map[string]any{"image": r.req.Image}))
	}
	var err error
	r.base, err = r.s.evaluationLaunchBase(r.excludeDeployment)
	if err != nil {
		r.out.err = internal(err)
		return false
	}
	// One materialization decides this deployment's launch configuration. The
	// preview below, the estimate, and a create submitted from this form all
	// read the same object.
	r.launch, err = materializeDeployRequestWithBase(r.eng.Name(), r.req, r.selection, requestTargetCards(r.profileNode, r.req), r.s.requestEngineVersion(r.eng.Name(), r.req, r.selection, r.targetVendor, r.profileNode), r.base)
	if r.errs.add(fieldExtraArgs, err) {
		// ProductErrors that name a key land on that key's control; the
		// extra-args parse error is the one that starts on the text box.
		r.writeFieldErrors()
		return false
	}
	r.quick = r.launch.Quick
	// Materialization parsed this text already. Compare the original user
	// tokens before any flags moved into fields or were derived for a task.
	submittedExtra, _ := deploy.ParseArgsText(r.req.ExtraArgs)
	var storedArgs *deploy.StoredLaunchConfig
	if r.base != nil {
		storedArgs = &r.base.Args
	}
	if command, err := effectiveRequestCommand(r.req, r.selection); err == nil && command.InterpretEngineDialect && !sameStoredExtraArgs(r.eng.Name(), submittedExtra, storedArgs) {
		r.resp.Warnings = append(r.resp.Warnings, deploy.ExtraArgsEngineWarnings(r.eng.Name(), r.launch.UserResidual)...)
	}
	priority := requestPriorityScheduling(r.req, r.eng.Name(), r.targetVendor, placementPD(r.req) != nil, r.launch.TemplateResidual, r.launch.UserResidual, r.selection.Entrypoint)
	r.resp.PriorityScheduling = &priority
	if _, priorityErr := ResolvePriorityScheduling(r.req.PriorityScheduling, priority); r.errs.add("priority_scheduling", priorityErr) {
		r.writeFieldErrors()
		return false
	}
	r.resp.EffectiveParams = launchConfigMap(r.quick)
	// FormValues is the same configuration read as a display instruction: the
	// effective values plus the argument text after the moved flags left it.
	// The form sets its fields from this and derives nothing itself.
	formValues := launchConfigMap(r.quick)
	formValues["extra_args"] = deploy.FormatArgsText(r.launch.UserResidual)
	r.resp.FormValues = formValues
	r.resp.Defaults = &dto.EvaluateDefaults{ModelMount: deploy.DefaultModelMount, MasterPort: deploy.DefaultMasterPort, Engine: deploy.DefaultEngine}
	r.resp.EngineDecidesParamKeys = r.launch.EngineDecidesParamKeys
	r.resp.Moved = r.launch.Moved
	r.resp.TemplateParams = r.launch.TemplateParams
	r.resp.TemplateResidualArgs = r.launch.TemplateResidual
	// The form values are already answered, so the deployer sees the share
	// the refusal is about and can lower it. An edit is not asked: the
	// deployment's own engine already holds the host memory in question.
	if r.base == nil && r.errs.add("sleepable", r.s.sleepHostBacking(r.eng.Name(), r.req, r.quick, r.profileNode)) {
		r.writeFieldErrors()
		return false
	}
	return true
}
