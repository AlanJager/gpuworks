package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// validateReplicaShape gives evaluation and create the same replica and rank
// contract before either path can register a model or acquire an image.
func validateReplicaShape(req *dto.DeployRequest) error {
	req.RoutingPolicy = deploy.NormalizeRoutingPolicy(req.RoutingPolicy)
	if !deploy.ValidRoutingPolicy(req.RoutingPolicy) {
		return productError(ErrorRoutingPolicyInvalid, "routing_policy is invalid", map[string]any{"routing_policy": req.RoutingPolicy})
	}
	if req.NNodes <= 0 {
		req.NNodes = 1
	}
	if req.Replicas < 0 {
		return productError(ErrorReplicasNegative, "replicas must be non-negative", nil)
	}
	if req.Replicas == 0 {
		req.Replicas = 1
	}
	if req.Replicas > planning.MaxReplicas {
		return productError(ErrorReplicasTooMany, fmt.Sprintf("replicas must be at most %d", planning.MaxReplicas), map[string]any{"maximum": planning.MaxReplicas})
	}
	if req.NNodes > 1 && req.Replicas != 1 {
		return productError(ErrorDistributedReplicaCount, "distributed deployments support exactly one replica", map[string]any{"required": 1})
	}
	return nil
}

// validateStartTimeout gives evaluation and create the same rule for a
// deployment's own health budget: unset, or inside the policy's bounds.
func validateStartTimeout(req *dto.DeployRequest) error {
	if req.StartTimeoutSec == 0 || store.ValidStartTimeoutSec(req.StartTimeoutSec) {
		return nil
	}
	return productError(ErrorFieldInvalid, "start_timeout_sec is out of range", map[string]any{
		"field":  "start_timeout_sec",
		"detail": fmt.Sprintf("start_timeout_sec must be 0 (use the cluster default) or between %d and %d seconds", store.DeploymentStartTimeoutSecLo, store.DeploymentStartTimeoutSecHi),
	})
}

// ---- deployments ----

func (a *apiHandlers) cloneDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	original, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	clone := *original
	clone.ID = 0
	clone.Name = original.Name + "-clone"
	clone.ServedName = withServedNameSuffix(original.ServedName)
	clone.State = store.DeployActive
	clone.DesiredSleep = 0
	clone.CreatedAt = time.Time{}
	clone.UpdatedAt = time.Time{}
	var override struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&override); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(override.Name) != "" {
		clone.Name = strings.TrimSpace(override.Name)
		clone.ServedName = clone.Name
	}
	if !validateName(w, clone.Name) || !validateTextField(w, "served_name", clone.ServedName, maxNameRunes) {
		return
	}
	var created *store.Deployment
	// The copy runs where the original may: its requirements are rows of
	// their own, which copying the row does not carry.
	reqs, err := a.d.Store.DeploymentRequirements(original.ID)
	if err == nil {
		created, err = a.d.Store.CreateDeploymentWithRequirements(&clone, reqs)
	}
	if err != nil {
		writeDeploymentCreateErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.deploymentDTO(created, true))
}

func (a *apiHandlers) createDeployment(w http.ResponseWriter, r *http.Request) {
	var req dto.DeployRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validateName(w, req.Name) || !validateTextField(w, "served_name", req.ServedName, maxNameRunes) {
		return
	}
	if strings.TrimSpace(req.TargetVendor) != "" && accelerator.NormalizeVendor(req.TargetVendor) == "" {
		writeProductError(w, http.StatusBadRequest, ErrorTargetVendorInvalid, "target_vendor is not supported", map[string]any{"vendor": req.TargetVendor})
		return
	}
	unitsAuto, err := a.autoPlaceRequest(&req)
	if err != nil {
		writeErr(w, unitPlacementStatus(err), err)
		return
	}
	defaultUnitStrategy(&req)
	if err := validateReplicaShape(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := validateStartTimeout(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := validateHealthCheckThreshold(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	pdReq, err := a.preparePDDeployRequest(&req, "")
	if err == nil && pdReq != nil {
		_, err = a.pdGroup(pdReq)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := a.pickAutomaticNode(r, &req); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	reqs, strategy, status, err := a.requestPlacement(&req)
	if err != nil {
		writeErr(w, status, err)
		return
	}
	profileNode, _ := a.d.Store.NodeWithGPUs(templateTargetNodeUUID(req.NodeUUID, req.NodeUUIDs))
	selection, err := a.applyTemplateDefaults(&req, nodeTemplateHardware(profileNode))
	if err == nil {
		err = checkPDSpec(selection, placementPD(&req))
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	eng, err := deploy.SelectEngine(req.Engine)
	if err == nil {
		err = pdReq.checkEngine(eng.Name())
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	preserveEngineModelName(&req)
	// The same materialization the deploy form previewed. What is stored from
	// here on is the launch configuration itself, never the four inputs it was
	// assembled from.
	targetVendor := accelerator.NormalizeVendor(req.TargetVendor)
	if targetVendor == "" {
		targetVendor = planning.RequirementVendor(reqs)
	}
	launch, err := a.materializeCreateLaunch(eng.Name(), &req, selection, deployTargetCards(profileNode, req.Placement), a.requestEngineVersion(eng.Name(), &req, selection, targetVendor, profileNode))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if pdReq != nil {
		if err := planning.CheckPDArgs(eng.Name(), launch.TemplateResidual, launch.UserResidual); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	// Resolve request-owned command, environment and mounts before template
	// model registration or image acquisition can mutate persistent state.
	modelMount, componentMounts := previewComponentMounts(a.d.Store, selection.Components, profileNode)
	// A multi-node group settles its nodes and parallelism first: an
	// unstated TP and PP default from the nodes, and the launch preflight
	// below measures the result. Nothing is persisted by this step.
	var distNodes []*store.Node
	if req.NNodes > 1 {
		distNodes, err = a.prepareDistributedDeployRequest(&req)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	placement, err := planning.ParsePlacement(req.Placement)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	nnodes := req.NNodes
	if nnodes <= 0 {
		nnodes = 1
	}
	parallel := placement.Parallelism(nnodes)
	perNode, err := parallelGPUsPerNode(parallel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	preflightParams := deploy.Params{
		ModelPath: modelMount, ServedName: req.ServedName, APIPort: planning.PreviewAPIPort,
		AssignedGPUs: perNode, TensorParallel: parallel.TP, PipelineParallel: parallel.PP, Sleepable: req.Sleepable, Quick: launch.Quick,
		TemplateArgs: launch.TemplateResidual, TemplateEnv: selection.Env, ExtraArgs: launch.UserResidual,
		NNodes: nnodes, MasterAddr: req.MasterAddr, MasterPort: req.MasterPort,
	}
	if profileNode != nil {
		// The bind address the launch will use, so the preflight resolves the
		// same command (planning.PlanLaunch sets it from the node too).
		preflightParams.Host = deploy.RuntimeBindHost(profileNode.RuntimeDriver)
		preflightParams.RuntimeDriver = profileNode.RuntimeDriver
	}
	pdReq.preflight(&preflightParams)
	preflight := deploy.ResolveLaunchInput{
		Engine: eng, Params: preflightParams, Image: req.Image,
		Entrypoint: selection.Entrypoint, PreStartArgv: selection.PreStartArgv,
		PreStartScript: selection.PreStartScript, RuntimeAssets: selection.Assets,
		ComponentMounts: componentMounts, APIContract: selection.APIContract,
		TemplateShmBytes: selection.shmBytes(), TemplateMemlockUnlimited: selection.memlockUnlimited(),
		AllowUnresolved: true,
	}
	imageNodes := append([]*store.Node{profileNode}, distNodes...)
	if pdReq != nil {
		imageNodes = append(imageNodes, pdReq.nodes...)
	}
	if err := a.admitImagePlatforms(eng.Name(), selection, req.Image, imageNodes); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	imageFor := func(node *store.Node) string {
		return a.rankImage(eng.Name(), selection, req.Image, node).Image
	}
	if err := preflightCreateRuntimes(preflight, req.ContainerOverrides, selection.Components, modelMount, profileNode, distNodes, pdReq, imageFor); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	model, err := a.resolveDeploymentModelFromTemplate(r.Context(), req.ModelID, &selection, req.TemplateID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	launch, err = materializeDeployRequest(eng.Name(), &req, selection, deployTargetCards(profileNode, req.Placement), a.requestEngineVersion(eng.Name(), &req, selection, targetVendor, profileNode))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	selection.ExtraArgs = launch.TemplateResidual
	quick, userExtraArgs := launch.Quick, deploy.FormatArgsText(launch.UserResidual)
	served := req.ServedName
	if served == "" {
		served = model.Name
	}
	name := req.Name
	if name == "" {
		name = served
	}
	if !validateTextField(w, "served_name", served, maxNameRunes) || !validateName(w, name) {
		return
	}
	model, templateConfig, err := a.initTemplateModelInferenceConfig(&selection, model)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The candidate's operations say what a check asks.
	healthCheck, healthThreshold, err := createHealthCheck(&req, model)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Warnings are surfaced by evaluate before submit; keep the same helper in
	// this path as a guard for API clients that submit without evaluating.
	_ = deploy.PerformanceWarnings(quick, userExtraArgs, model.Arch, 0)
	if pdReq != nil {
		distNodes = pdReq.otherNodes()
	}
	var node *store.Node
	if code := planning.SleepDeclarable(req.Sleepable, eng.Name()); code != "" {
		writeProductError(w, http.StatusBadRequest, code, sleepRefusalFallback(code), map[string]any{"engine": eng.Name()})
		return
	}
	yieldPolicy, ok := createYieldPolicy(w, req, eng.Name(), pdReq != nil)
	if !ok {
		return
	}
	if req.NNodes > 1 && pdReq == nil {
		node, err = a.d.Store.NodeWithGPUs(distNodes[0].UUID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	} else {
		node, err = a.requestNode(req.NodeUUID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
	}
	req.PartitionBackend, err = resolvePartitionBackend(node, req.VRAMLimitGiB, req.PartitionBackend)
	if err != nil {
		writeErr(w, http.StatusBadRequest, wrapProductError(ErrorDistributedPartitionBackendUnavailable, err.Error(), map[string]any{"node": node.UUID, "detail": err.Error()}, err))
		return
	}
	for _, distNode := range distNodes {
		if _, err := resolvePartitionBackend(distNode, req.VRAMLimitGiB, req.PartitionBackend); err != nil {
			message := fmt.Sprintf("node %s: %s", distNode.UUID, err)
			writeErr(w, http.StatusBadRequest, wrapProductError(ErrorDistributedPartitionBackendUnavailable, message, map[string]any{"node": distNode.UUID, "detail": err.Error()}, err))
			return
		}
	}
	if err := a.admitImagePlatforms(eng.Name(), selection, req.Image, append([]*store.Node{node}, distNodes...)); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	weightStorage, err := a.resolveSaveToStorage(req.ModelSource, model, append([]*store.Node{node}, distNodes...))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	// What the request names is what the deployment keeps: 0 leaves where a
	// node saves a model it must fetch to the default order.
	saveTo := a.previewSaveToStorageID(req.ModelSource, model, node)
	rules := store.SourceRules{Pinned: store.StoragePins(reqs), SaveTo: saveTo}
	if err := a.admitDeploymentModel(r.Context(), model, rules, append([]*store.Node{node}, distNodes...)); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	imageResolution := a.rankImage(eng.Name(), selection, req.Image, node)
	if strings.TrimSpace(imageResolution.Image) == "" {
		code, params := ErrorAcquisitionImageRequired, map[string]any(nil)
		if len(imageResolution.Warnings) > 0 {
			code, params = imageResolution.Warnings[0].Code, imageResolution.Warnings[0].Params
		}
		writeProductError(w, http.StatusBadRequest, code, "no runtime image is available for the target node", params)
		return
	}
	req.Image = imageResolution.Image
	reqs = planning.WithNodeVendor(reqs, node)
	if err := a.checkCreateCanEverFit(r.Context(), &req, pdReq != nil, reqs, eng, model, selection, placement, quick, launch.UserResidual, modelMount, componentMounts); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// An automatically placed create re-runs the plan the evaluation answered
	// with: resources may have moved since, and the answer is the same
	// conflict the client handles for stale labels - not a doomed row.
	if isAutomaticPlacement(&req) && req.NNodes <= 1 && pdReq == nil {
		requestOverrides, _ := requestContainerOverrides(req.ContainerOverrides)
		if err := a.revalidateAutomaticPlacement(r.Context(), &req, eng, model, selection, placement, quick, launch.UserResidual, strings.TrimSpace(req.ServedName), node, modelMount, componentMounts, requestOverrides, saveTo); err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
	}
	snapshot, err := catalogSelectionDocument(selection, req.Image)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dep := &store.Deployment{
		Name:                     name,
		ModelID:                  model.ID,
		TemplateID:               req.TemplateID,
		TemplateSpecSource:       req.TemplateSpecSource,
		TemplateSpecName:         req.TemplateSpecName,
		TemplateScenario:         req.Scenario,
		TemplateDeviated:         selection.Deviated,
		CatalogSelectionSnapshot: snapshot,
		NodeID:                   node.ID,
		ServedName:               served,
		SaveToStorageID:          saveTo,
		Engine:                   eng.Name(),
		Image:                    req.Image,
		QuickParams:              launchConfigJSON(quick),
		ExtraArgs:                userExtraArgs,
		ContainerOverrides:       jsonStr(req.ContainerOverrides),
		Placement:                jsonStr(storedPlacement(req.Placement, isAutomaticPlacement(&req) && req.NNodes <= 1 && pdReq == nil)),
		PlacementStrategy:        strategy,
		UnitsAuto:                unitsAuto,
		DistGroup:                req.DistGroup,
		DistRole:                 req.DistRole,
		NNodes:                   req.NNodes,
		NodeRank:                 req.NodeRank,
		MasterAddr:               req.MasterAddr,
		MasterPort:               req.MasterPort,
		NodeIDs: func() string {
			ids := []uint{node.ID}
			if req.NNodes > 1 {
				ids = ids[:0]
			}
			for _, n := range distNodes {
				ids = append(ids, n.ID)
			}
			return jsonStr(ids)
		}(),
		Sleepable:               req.Sleepable,
		HealthCheck:             healthCheck,
		HealthCheckThresholdSec: healthThreshold,
		TokenizeDisabled:        req.TokenizeEnabled != nil && !*req.TokenizeEnabled,
		VRAMLimitBytes:          int64(req.VRAMLimitGiB * (1 << 30)),
		StartTimeoutSec:         req.StartTimeoutSec,
		PartitionBackend:        req.PartitionBackend,
		Replicas:                req.Replicas,
		RoutingPolicy:           req.RoutingPolicy,
		State:                   store.DeployActive,
	}
	dep.SetPolicy(yieldPolicy)
	var created *store.Deployment
	if templateConfig.modelID == 0 {
		created, err = a.d.Store.CreateDeploymentWithRequirements(dep, reqs)
	} else {
		created, err = a.d.Store.CreateDeploymentWithTemplateInferenceConfig(dep, reqs, templateConfig.modelID, templateConfig.operations, templateConfig.inputModalities)
	}
	if err != nil {
		writeDeploymentCreateErr(w, err)
		return
	}
	// Start the download only after validation and persistence: a rejected
	// create must not leave an unowned transfer running.
	if status, err := a.startSharedModelDownload(model, weightStorage, append([]*store.Node{node}, distNodes...), created.ID); err != nil {
		_ = a.d.Store.DeleteDeployment(created.ID)
		writeErr(w, status, err)
		return
	}
	writeJSON(w, http.StatusCreated, a.deploymentDTO(created, true))
}

func (a *apiHandlers) getDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, a.deploymentDTO(dep, true))
}

func (a *apiHandlers) setRoutingPolicy(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var body struct {
		RoutingPolicy string `json:"routing_policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	policy := deploy.NormalizeRoutingPolicy(body.RoutingPolicy)
	if !deploy.ValidRoutingPolicy(policy) {
		writeProductError(w, http.StatusBadRequest, ErrorRoutingPolicyInvalid, "routing_policy is invalid", map[string]any{"routing_policy": policy})
		return
	}
	if err := a.d.Store.SetDeploymentRoutingPolicy(dep.ID, policy); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	dep.RoutingPolicy = policy
	writeJSON(w, http.StatusOK, a.deploymentDTO(dep, true))
}

func (a *apiHandlers) stopDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	if err := a.d.Store.SetDeploymentState(id, store.DeployStopped); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

func (a *apiHandlers) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	id := parseUint(chi.URLParam(r, "id"))
	dep, err := a.d.Store.GetDeployment(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if r.URL.Query().Get("force") == "true" {
		// A force delete guarantees that the CONTROL-PLANE record goes away,
		// even when the node is unreachable. It must still give the node a
		// chance to tear the container down on the way out: sending nothing
		// left the container running with its GPU allocated and no deployment
		// left to explain it (measured at 37.4 GiB), and nothing would ever
		// reap it.
		//
		// The node is told by re-syncing its desired state, which is the
		// existing mechanism for this: agent-side SetDesiredFull walks the
		// instances it had and tears down every one that is no longer in the
		// desired set. That has to happen AFTER the row is gone, or the
		// re-sync would still be describing the deployment as live. It is
		// best-effort and not waited on - that is what force means - so an
		// unreachable node is logged and the delete proceeds regardless.
		nodeUUIDs := map[string]bool{}
		if insts, err := a.d.Store.InstancesForDeployment(id); err == nil {
			for _, inst := range insts {
				if node, err := a.d.Store.NodeByID(inst.NodeID); err == nil && node.UUID != "" {
					nodeUUIDs[node.UUID] = true
				}
			}
		}
		if err := a.d.Store.DeleteDeployment(id); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		for uuid := range nodeUUIDs {
			if err := a.d.NodeSync.SyncNode(uuid); err != nil && a.d.Log != nil {
				a.d.Log.Warn("force delete: could not re-sync node to reap the container",
					"deployment", id, "node", uuid, "err", err)
			}
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
		return
	}
	if dep.State != store.DeployDeleting {
		if err := a.d.Store.SetDeploymentState(id, store.DeployDeleting); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deleting"})
}

// evaluate previews the assembled vllm command line for the deploy form.
