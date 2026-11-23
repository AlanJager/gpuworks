package deployment

import (
	"context"
	"errors"
	"slices"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// materializeLaunch decides the launch configuration and, for a multi-node
// group, its nodes and parallelism.
func (c *createState) materializeLaunch() error {
	var err error
	// The same materialization the deploy form previewed. What is stored from
	// here on is the launch configuration itself, never the four inputs it was
	// assembled from.
	c.targetVendor = accelerator.NormalizeVendor(c.req.TargetVendor)
	if c.targetVendor == "" {
		c.targetVendor = planning.RequirementVendor(c.reqs)
	}
	cards := requestTargetCards(c.profileNode, c.req)
	version := c.s.requestEngineVersion(c.eng.Name(), c.req, c.selection, c.targetVendor, c.profileNode)
	c.launch, err = c.s.materializeCreateLaunch(c.eng.Name(), c.req, c.selection, cards, version)
	if err != nil {
		return invalid(err)
	}
	if c.pdReq != nil {
		if err := planning.CheckPDArgs(c.eng.Name(), c.launch.TemplateResidual, c.launch.UserResidual); err != nil {
			return invalid(err)
		}
	}
	// Settle distributed parallelism before preflight and persistence.
	c.dp, err = deploy.PlacementDataParallel(c.eng.Name(), c.launch.Quick.DP, c.launch.TemplateResidual, c.launch.UserResidual)
	if err != nil {
		return invalid(err)
	}
	if c.req.NNodes > 1 {
		if needsDistributedRecommendation(c.req, c.unitsAuto) {
			preview := c.selection
			preview.previewWeights = true
			preview.Components = slices.Clone(c.selection.Components)
			model, resolveErr := c.s.resolveDeploymentModelFromTemplate(c.ctx, c.req.ModelID, &preview, c.req.TemplateID)
			if resolveErr != nil {
				return invalid(resolveErr)
			}
			candidateLaunch, launchErr := materializeDeployRequest(c.eng.Name(), c.req, preview, cards, version)
			if launchErr != nil {
				return invalid(launchErr)
			}
			c.distNodes, err = c.s.recommendDistributed(c.ctx, c.req, c.dp, c.eng, candidateLaunch, preview, model)
		} else {
			c.distNodes, err = c.s.PrepareDistributedDeployRequest(c.req, c.dp)
		}
		// A request cancelled or out of time during the recommendation's
		// trials is not a wrong request.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err != nil {
			return invalid(err)
		}
	}
	return nil
}

// runPreflight resolves the request-owned command, environment and mounts
// before anything is written.
func (c *createState) runPreflight() error {
	var err error
	// Resolve request-owned command, environment and mounts before template
	// model registration or image acquisition can mutate persistent state.
	// Evaluate runs the same preflight (evaluateLaunchPreflight).
	c.preflight, err = c.s.prepareLaunchPreflight(c.eng, c.req, c.selection, c.launch, c.dp, c.targetVendor, c.profileNode, c.pdReq)
	if err != nil {
		return invalid(err)
	}
	c.modelMount, c.componentMounts, c.placement = c.preflight.modelMount, c.preflight.componentMounts, c.preflight.placement
	imageNodes := append([]*store.Node{c.profileNode}, c.distNodes...)
	if c.pdReq != nil {
		imageNodes = append(imageNodes, c.pdReq.nodes...)
	}
	if err := c.s.admitImagePlatforms(c.eng.Name(), c.selection, c.req.Image, imageNodes); err != nil {
		return invalid(err)
	}
	imageFor := func(node *store.Node) string {
		return c.s.rankImage(c.eng.Name(), c.selection, c.req.Image, node).Image
	}
	if err := c.preflight.run(c.req, c.selection, c.profileNode, c.distNodes, c.pdReq, imageFor); err != nil {
		return invalid(err)
	}
	return nil
}

// resolveModel registers the model the template names, materializes the
// launch again with it, and settles the names, health check and priority.
func (c *createState) resolveModel() error {
	var err error
	c.model, err = c.s.resolveDeploymentModelFromTemplate(c.ctx, c.req.ModelID, &c.selection, c.req.TemplateID)
	if err != nil {
		return invalid(err)
	}
	c.launch, err = materializeDeployRequest(c.eng.Name(), c.req, c.selection, requestTargetCards(c.profileNode, c.req), c.s.requestEngineVersion(c.eng.Name(), c.req, c.selection, c.targetVendor, c.profileNode))
	if err != nil {
		return invalid(err)
	}
	c.selection.ExtraArgs = c.launch.TemplateResidual
	c.quick, c.userExtraArgs = c.launch.Quick, deploy.FormatArgsText(c.launch.UserResidual)
	c.served = c.req.ServedName
	if c.served == "" {
		c.served = c.model.Name
	}
	c.name = c.req.Name
	if c.name == "" {
		c.name = c.served
	}
	// The served name is judged first, as the name defaults to it.
	if err := TextFieldError("served_name", c.served, MaxNameRunes); err != nil {
		return invalid(err)
	}
	if err := TextFieldError("name", c.name, MaxNameRunes); err != nil {
		return invalid(err)
	}
	c.model, c.templateConfig, err = c.s.initTemplateModelInferenceConfig(&c.selection, c.model)
	if err != nil {
		return internal(err)
	}
	// The candidate's operations say what a check asks.
	c.healthCheck, c.healthThreshold, err = CreateHealthCheck(c.req, c.model)
	if err != nil {
		return invalid(err)
	}
	c.priorityScheduling, err = ResolvePriorityScheduling(c.req.PriorityScheduling, requestPriorityScheduling(c.req, c.eng.Name(), c.targetVendor, c.pdReq != nil, c.launch.TemplateResidual, c.launch.UserResidual, c.selection.Entrypoint))
	if err != nil {
		return invalid(err)
	}
	// Warnings are surfaced by evaluate before submit; keep the same helper in
	// this path as a guard for API clients that submit without evaluating.
	_ = deploy.PerformanceWarnings(c.quick, c.userExtraArgs, c.model.Arch, 0)
	return nil
}
