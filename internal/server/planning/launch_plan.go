// launch_plan.go assembles one deployment's launch: the estimate and schedule
// pass, the engine parameters, the image facts and the resolved command. The
// api's preview and the controller's scheduler both used to do this by hand,
// which is how the two ended up estimating from different inputs.

package planning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/placement"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// PreviewAPIPort is the placeholder a preview shows where a real launch has the
// port the scheduler reserved for it.
const PreviewAPIPort = 30000

// LaunchRequest is one launch assembly's input: the rows and choices the caller
// owns. Both callers build it from what they have - the controller from stored
// rows, the preview from a request - and neither assembles a launch itself.
type LaunchRequest struct {
	// Preview marks an offline preview: no port is reserved and no GPU assigned,
	// and a placement that cannot be resolved is still previewed symbolically.
	Preview bool
	// Unloaded previews the node as if nothing ran on it (Input.Unloaded).
	Unloaded bool
	// ExcludeDeploymentID previews the node without that deployment's
	// instances (Input.ExcludeDeploymentID).
	ExcludeDeploymentID uint
	// APIPort is the reserved host port of a real launch; a preview passes zero
	// and gets PreviewAPIPort.
	APIPort int

	Engine    string
	Model     *store.Model
	Node      *store.Node
	Selection deploy.CatalogSelection
	Placement Placement
	Topology  DistTopology
	Source    LaunchSource
	Quick     deploy.LaunchConfig
	ExtraArgs []string

	// ModelMount is the primary component's container path; ModelHostPath is the
	// host directory behind it (empty in a preview, which mounts nothing).
	ModelMount      string
	ModelHostPath   string
	ComponentMounts []proto.MountSpec

	ServedName string
	Sleepable  bool
	// YieldPolicy is the deployment's (servicelevel): the cards of
	// deployments it does not share with stay reserved against it even while
	// they sleep. Empty reads as never.
	YieldPolicy        string
	MasterAddr         string
	MasterPort         int
	VRAMLimitBytes     int64
	PartitionBackend   string
	RuntimeImageCached *bool
	// APIContract is the contract the resolved launch advertises, already
	// reduced by the caller's rule for model operations versus the snapshot.
	APIContract json.RawMessage
	// PD is set on a PD disaggregated instance: its role and side channel
	// (ADR 0028). Quick is then already the role's configuration.
	PD *deploy.PDLaunch
	// ModelRules are where the model comes from on the node: the storages
	// the deployment's requirements limit it to and the one it saves into
	// (store.ModelSourceOn).
	ModelRules store.SourceRules
}

// LaunchPlan is one assembled launch and everything it was built from, so the
// caller persists or renders it without assembling anything again.
type LaunchPlan struct {
	// Input is the estimate input the plan was built from. It is a read-only
	// projection for callers that need to compare two paths (the preview and the
	// controller); nobody may edit it and feed it back, or the comparison would
	// stop describing what the server actually planned.
	Input    Input
	Output   Output
	Params   deploy.Params
	Resolved deploy.ResolvedLaunch
	// Warnings are what the platform cannot prove about the resolved launch.
	// They are for the preview to show; a plan never fails on them.
	Warnings  []messages.Warning
	ImagePull ImagePullPlan
	Topology  DistTopology
	// Source is the launch-side half the plan was built from, kept so a caller
	// persists exactly what it assembled.
	Source LaunchSource
	// Profile is the engine profile the launch uses (health path, sleep support,
	// metrics, shared memory).
	Profile deploy.Profile
}

// PlanLaunch estimates, schedules and assembles one launch on one node.
//
// A schedule that does not fit is not an error: the caller reads
// LaunchPlan.Output and decides what to say. The one exception is a placement
// error on a real launch, which no caller can act on, and a preview that cannot
// resolve its placement is still assembled with the symbolic values the form
// shows.
func PlanLaunch(ctx context.Context, st *store.Store, networks Networks, req LaunchRequest) (LaunchPlan, error) {
	_ = ctx
	if req.Model == nil || req.Node == nil {
		return LaunchPlan{}, fmt.Errorf("launch plan requires a model and a node")
	}
	input, err := launchInput(st, req)
	if err != nil {
		return LaunchPlan{}, err
	}
	input.ServingUnavailable = !networks.CanServe(*req.Node)
	plan := LaunchPlan{Input: input, Output: input.Evaluate(st), Topology: req.Topology, Source: req.Source}
	if plan.Output.PlacementError != nil && !req.Preview {
		return plan, fmt.Errorf("deployment %d placement: %w", req.Source.DeploymentID, plan.Output.PlacementError)
	}
	if (!plan.Output.Result.OK || plan.Output.Result.Plan == nil) && !req.Preview {
		return plan, nil
	}

	gpus, assigned, parallel := previewPlacement(req, plan.Output)
	gpu, err := launchGPU(req.Node, gpus)
	if err != nil {
		return plan, err
	}
	profile, err := deploy.ProfileForEngine(req.Engine)
	if err != nil {
		return plan, err
	}
	plan.Profile = profile
	imagePull, err := ImagePullPlanFor(st, nodeResolverOf(networks), req.Source, req.Node, req.Selection)
	if err != nil {
		return plan, err
	}
	plan.ImagePull = imagePull

	apiPort := req.APIPort
	if req.Preview {
		apiPort = PreviewAPIPort
	}
	if req.Topology.Headless {
		apiPort = 0
	}
	arch := req.Node.ArchOf(gpu)
	params := deploy.Params{
		ModelPath: req.ModelMount, ServedName: req.ServedName, APIPort: apiPort,
		AssignedGPUs: assigned, TensorParallel: parallel.TP, PipelineParallel: parallel.PP,
		Sleepable: req.Sleepable, Quick: req.Quick,
		TemplateArgs: req.Selection.ExtraArgs, TemplateEnv: req.Selection.Env, ExtraArgs: req.ExtraArgs,
		Arch: arch.Family, ArchID: arch.ID, ComputeCapability: gpu.ComputeCapability, Vendor: gpu.Vendor,
		ModelType: req.Model.Arch,
		NNodes:    req.Topology.NNodes, NodeRank: req.Topology.NodeRank,
		MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
		Headless: req.Topology.Headless, NodeAddress: req.Topology.NodeAddress,
		ComputeNetwork: req.Topology.Network,
		PD:             req.PD,
		Host:           deploy.RuntimeBindHost(req.Node.RuntimeDriver),
		RuntimeDriver:  req.Node.RuntimeDriver,
	}
	plan.Params = params

	resolver := req.Selection.PlacementResolver()
	// A placement the scheduler could not settle is still previewed: the
	// variables the topology resolver would fill stay symbolic, which is what
	// the form shows and what the persisted launch replaces once a node is
	// chosen.
	unresolved := !plan.Output.Result.OK || plan.Output.Result.Plan == nil
	var variables map[string]any
	if resolver != "" && !unresolved {
		variables = placementVariables(plan.Output, len(gpus), parallel.TP, req.ModelMount)
	}
	engine, err := deploy.SelectEngine(req.Engine)
	if err != nil {
		return plan, err
	}
	resolved, warnings, err := deploy.ResolveLaunchWithWarnings(deploy.ResolveLaunchInput{
		Engine: engine, Params: params, Image: req.Source.Image,
		ContainerOverrides: req.Source.Overrides, ComponentModelMount: componentModelMount(req),
		Entrypoint: req.Selection.Entrypoint, PreStartArgv: req.Selection.PreStartArgv,
		PreStartScript: req.Selection.PreStartScript, RuntimeAssets: req.Selection.RuntimeAssets,
		ComponentMounts: req.ComponentMounts, TemplateShmBytes: req.Selection.ShmBytes, TemplateMemlockUnlimited: req.Selection.MemlockUnlimited,
		APIContract: req.APIContract, PlacementVariables: variables,
		AllowUnresolved: req.Preview && unresolved,
		HealthPath:      profile.HealthPath,
		// The KV disk tier's automatic capacity ceiling is derived from the
		// weights the estimator already sized for this deployment; it reaches
		// the agent as the mount's size hint and is never written back into the
		// deployment's configuration.
		ModelWeightBytes: plan.Output.Breakdown.WeightBytes,
	})
	if err != nil {
		return plan, fmt.Errorf("deployment %d resolve launch: %w", req.Source.DeploymentID, err)
	}
	plan.Resolved = resolved
	plan.Warnings = warnings
	return plan, nil
}

// launchInput is the estimate and schedule input for one launch. It is the same
// for the preview and the controller by construction: both spend the catalog's
// spec and runtime weights, its measured KV cost, and the node's cache fact.
func launchInput(st *store.Store, req LaunchRequest) (Input, error) {
	componentWeightBytes, componentArtifacts := componentSum(req.Selection.Components)
	return Input{
		Model: req.Model, Node: req.Node, Engine: req.Engine, Quick: req.Quick,
		SpecWeightBytes:      req.Selection.WeightBytes,
		RuntimeWeightBytes:   req.Selection.RuntimeWeightBytes,
		ComponentWeightBytes: componentWeightBytes,
		ComponentArtifacts:   componentArtifacts,
		KVBytesPerToken:      CatalogKVBytesPerToken(st, nil, req.Model, req.Engine),
		RuntimeImageBytes:    CatalogImageBytes(st, req.Source.Image),
		RuntimeImageCached:   runtimeImageCached(st, req),
		Unloaded:             req.Unloaded,
		ExcludeDeploymentID:  req.ExcludeDeploymentID,
		YieldPolicy:          req.YieldPolicy,
		Parallel:             req.Placement.Parallelism(req.Topology.NNodes),
		GPUIndexes:           req.Placement.GPUIndexes,
		VRAMLimitBytes:       req.VRAMLimitBytes,
		PartitionBackend:     req.PartitionBackend,
		Sleepable:            req.Sleepable,
		PlacementResolver:    req.Selection.PlacementResolver(),
		ModelRules:           req.ModelRules,
	}, nil
}

// runtimeImageCached is the node's cache fact for the runtime image. A caller
// that asked the node passes what it learned; otherwise the store's image_nodes
// record answers, which is all the controller has.
func runtimeImageCached(st *store.Store, req LaunchRequest) *bool {
	if req.RuntimeImageCached != nil {
		return req.RuntimeImageCached
	}
	if st == nil || strings.TrimSpace(req.Source.Image) == "" {
		return nil
	}
	image, err := st.ResolveImageRef(req.Source.Image)
	if err != nil {
		return nil
	}
	node, err := st.ImageNode(image.ID, req.Node.ID)
	if err != nil {
		return nil
	}
	cached := node.State == store.ImageStatePresent
	return &cached
}

// previewPlacement is what one rank launches with: its GPUs, how many GPUs
// that is, and the deployment's parallelism. A schedule that resolved gives
// the GPUs; a preview without one keeps the GPUs the request pinned and the
// per-node count the stated parallelism implies (or one).
func previewPlacement(req LaunchRequest, out Output) ([]int, int, parallelism.Spec) {
	spec := req.Placement.Parallelism(req.Topology.NNodes)
	if out.Result.OK && out.Result.Plan != nil {
		return out.Result.Plan.GPUIndexes, len(out.Result.Plan.GPUIndexes), spec.Assigned(out.Result.Plan.TP)
	}
	perNode, err := spec.GPUsPerNode()
	if err != nil || perNode < 1 {
		perNode = 1
	}
	if len(req.Placement.GPUIndexes) > 0 {
		perNode = len(req.Placement.GPUIndexes)
	}
	return req.Placement.GPUIndexes, perNode, spec.Assigned(perNode)
}

// launchGPU resolves the card a launch was assigned. The scheduler names GPUs
// by hardware index - the card's own identity, not its position in the node's
// report - so a card that has since disappeared is an error, never a wrong
// card. A preview with no assigned index takes the first card the node reports,
// which is the best fact an unresolved placement has.
func launchGPU(node *store.Node, indexes []int) (store.GPU, error) {
	if node == nil {
		return store.GPU{}, fmt.Errorf("launch requires a node")
	}
	if len(indexes) == 0 {
		owned := node.OwnedGPUs()
		if len(owned) == 0 {
			return store.GPU{}, nil
		}
		return owned[0], nil
	}
	gpu, ok := node.GPUByIndex(indexes[0])
	if !ok {
		return store.GPU{}, fmt.Errorf("node %s has no GPU with index %d", node.UUID, indexes[0])
	}
	return gpu, nil
}

func componentSum(components []deploy.ComponentBinding) (int64, []Artifact) {
	var weightBytes int64
	var artifacts []Artifact
	for _, component := range components {
		if component.Role == "primary" || component.WeightBytes <= 0 {
			continue
		}
		weightBytes += component.WeightBytes
		artifacts = append(artifacts, Artifact{ModelID: component.StoreModelID, StorageBytes: component.WeightBytes})
	}
	return weightBytes, artifacts
}

func placementVariables(out Output, gpuCount, tp int, modelMount string) map[string]any {
	return placement.RuntimeVariables(out.Placement, gpuCount, tp, modelMount)
}

// componentModelMount is the template component's primary mount a launch's
// weights go to, or "" when the selection has no components and the default
// mount applies.
func componentModelMount(req LaunchRequest) string {
	if len(req.Selection.Components) == 0 {
		return ""
	}
	return req.ModelMount
}
