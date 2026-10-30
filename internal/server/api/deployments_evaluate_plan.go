package api

import (
	"context"
	"net/http"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	placementeval "gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// evaluatePlanEnv is the state the preview's single plan call reads.
type evaluatePlanEnv struct {
	ctx               context.Context
	writer            http.ResponseWriter
	request           *dto.DeployRequest
	excludeDeployment uint
	response          *dto.EvaluateResponse
	engine            deploy.Engine
	model             *store.Model
	node              *store.Node
	nodeFound         bool
	profileNode       *store.Node
	imageCached       *bool
	nodeImageCached   *bool
	selection         templateSelection
	placement         planning.Placement
	quick             deploy.LaunchConfig
	userExtraArgs     []string
	servedName        string
	requestOverrides  deploy.ContainerOverrides
	modelMount        string
	componentMounts   []proto.MountSpec
	gpuCount          int
	resolver          string
	templateResidual  []string
	errs              *evaluateFieldErrors
}

// evaluatePlanResult is what the plan leaves behind: the pieces of handler
// state the projection updates, in one place.
type evaluatePlanResult struct {
	nodeImageCached    *bool
	placement          placementeval.Result
	placementResolved  bool
	modelWeightBytes   int64
	placementVariables map[string]any
	allowUnresolved    bool
	dynamicTPPreview   bool
	gpuCount           int
	tp                 int
	quick              deploy.LaunchConfig
}

// evaluatePlan runs the one plan this preview shows and projects its output
// onto the response. It reports false when it has already written an error.
func (a *apiHandlers) evaluatePlan(env evaluatePlanEnv) (evaluatePlanResult, bool) {
	req, resp, node := env.request, env.response, env.node
	// tp is the deployment's tensor-parallel degree: stated, or completed
	// from the GPUs the schedule picks (package parallelism).
	out := evaluatePlanResult{nodeImageCached: env.nodeImageCached, gpuCount: env.gpuCount, tp: env.placement.TP, quick: env.quick}
	if env.nodeFound {
		if env.profileNode != nil && env.profileNode.UUID == node.UUID {
			out.nodeImageCached = env.imageCached
		}
		specWeightBytes := int64(0)
		if env.selection.Spec != nil {
			specWeightBytes = env.selection.Spec.WeightBytes
		}
		a.persistCatalogWeight(env.model, specWeightBytes)
		previewSelection := snapshotTemplateSelection(env.selection, resp.Image)
		previewSelection.ExtraArgs = env.templateResidual
		previewPlan, planErr := planning.PlanLaunch(env.ctx, a.d.Store, a.networks(), planning.LaunchRequest{
			Preview: true, ExcludeDeploymentID: env.excludeDeployment, Engine: env.engine.Name(), Model: env.model, Node: node,
			Selection: previewSelection, Placement: env.placement, Quick: env.quick, ExtraArgs: env.userExtraArgs,
			// Rank zero's share of a multi-node group: the node count decides
			// how many of the TP×PP GPUs land here.
			Topology:   planning.DistTopology{NNodes: max(req.NNodes, 1), Role: "standalone"},
			Source:     planning.LaunchSourceFromRequest(resp.Image, env.requestOverrides),
			ModelMount: env.modelMount, ComponentMounts: env.componentMounts,
			ServedName: env.servedName, Sleepable: req.Sleepable, YieldPolicy: requestYield(req.YieldPolicy),
			MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
			VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: req.PartitionBackend,
			RuntimeImageCached: out.nodeImageCached, APIContract: previewSelection.APIContract,
			ModelRules: store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: a.previewSaveToStorageID(req.ModelSource, env.model, node)},
		})
		if planErr != nil {
			// Launch-assembly failures name no form control; they belong on the
			// page-level error line, not beside a parameter.
			env.errs.add("", planErr)
			writeEvaluateFieldErrors(env.writer, env.response, env.errs)
			return evaluatePlanResult{}, false
		}
		if previewPlan.Output.PlacementError != nil {
			env.errs.add("", previewPlan.Output.PlacementError)
			writeEvaluateFieldErrors(env.writer, env.response, env.errs)
			return evaluatePlanResult{}, false
		}
		// Everything the form shows about the estimate comes from the plan the
		// launch would use: one input, one breakdown, one schedule.
		out.placement = previewPlan.Output.Placement
		out.modelWeightBytes = previewPlan.Output.Breakdown.WeightBytes
		resp.Breakdown = previewPlan.Output.Breakdown
		result := previewPlan.Output.Result
		if !previewPlan.Output.Fits() {
			// Why it does not fit, as the controller would say it: waiting for
			// holders to yield, or for sleepers' cards, reads as such.
			result.Message = previewPlan.Output.Refusal(node, servicelevel.Policy{Yield: requestYield(req.YieldPolicy)})
		}
		resp.Plan = result
		if len(previewPlan.Output.Eviction) > 0 {
			resp.Eviction = make([]dto.EvictionSuggestion, 0, len(previewPlan.Output.Eviction))
			for _, suggestion := range previewPlan.Output.Eviction {
				resp.Eviction = append(resp.Eviction, dto.EvictionSuggestion{
					InstanceUUID: suggestion.InstanceUUID,
					DeploymentID: suggestion.DeploymentID,
					Deployment:   suggestion.Deployment,
					GPUIndexes:   suggestion.GPUIndexes,
					FreesGiB:     suggestion.FreesGiB,
				})
			}
		}
		resp.Warnings = append(resp.Warnings, previewPlan.Output.Warnings...)
		if !previewPlan.Output.Result.OK {
			resp.OK = false
		}
		if previewPlan.Output.Result.OK && previewPlan.Output.Result.Plan != nil {
			out.gpuCount = len(previewPlan.Output.Result.Plan.GPUIndexes)
			out.tp = env.placement.Parallelism(req.NNodes).Assigned(out.gpuCount).TP
			out.placementResolved = true
		}
		out.quick.CPUCores = node.CPUCores
		out.quick.EstimatedGPUKVGiB = float64(previewPlan.Output.Breakdown.KVTotalBytes) / float64(int64(1)<<30)
	}
	if env.resolver != "" && env.nodeFound && out.placementResolved {
		out.placementVariables = placementeval.RuntimeVariables(out.placement, out.gpuCount, out.tp, env.modelMount)
	}
	out.allowUnresolved = !out.placementResolved
	out.dynamicTPPreview = out.tp <= 0 && (!env.nodeFound || !out.placementResolved)
	resp.Warnings = append(resp.Warnings, deploy.PerformanceWarnings(out.quick, deploy.FormatArgsText(env.userExtraArgs), env.model.Arch, out.gpuCount)...)
	if out.tp <= 0 {
		out.tp = 1
	}
	return out, true
}
