package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fieldPDPlacement is the deploy form control PD placement errors belong to.
const fieldPDPlacement = "placement.pd"

// pdRequest is a PD deploy request after validation: the placement, its mode,
// and the nodes it names in the mode's role order (decode first), with GPUs.
type pdRequest struct {
	pd        *planning.PDPlacement
	mode      pdmode.Mode
	nodes     []*store.Node
	warnings  []messages.Warning
	sleepable bool
}

// placementPD returns the request's PD section, or nil for any other shape.
// A placement that cannot be read is left to the ordinary placement check,
// which rejects it.
func placementPD(req *dto.DeployRequest) *planning.PDPlacement {
	pd, _ := planning.PDOf(req.Placement)
	return pd
}

// preparePDDeployRequest validates a PD request against its mode and the
// nodes it names (the one check planning owns), and points the request's
// primary node at the first of them. A PD deployment is its own shape: it
// states its instances per role, so a multi-node group, a node list or a
// replica count beside it is a conflict rather than something to merge.
func (a *apiHandlers) preparePDDeployRequest(req *dto.DeployRequest, engine string) (*pdRequest, error) {
	pd := placementPD(req)
	if pd == nil {
		return nil, nil
	}
	if req.NNodes > 1 || len(req.NodeUUIDs) > 0 || req.Replicas > 1 || isAutomaticPlacement(req) {
		return nil, productError(messages.ErrorPDShapeConflict, "a PD deployment states its instances per role; nnodes, node_uuids and replicas do not apply", nil)
	}
	mode, err := planning.ValidatePD(pd, engine)
	if err != nil {
		return nil, err
	}
	reqs, err := requestRequirements(req)
	if err != nil {
		return nil, err
	}
	byUUID := map[string]*store.Node{}
	var nodes []*store.Node
	for _, uuid := range pd.NodeUUIDs(mode) {
		node, err := a.d.Store.NodeWithGPUs(uuid)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // ValidatePDNodes names the missing node
			}
			return nil, err
		}
		byUUID[uuid] = node
		nodes = append(nodes, node)
	}
	if err := a.matchNodes(nodes, reqs); err != nil {
		return nil, err
	}
	warnings, err := planning.ValidatePDNodes(pd, mode, byUUID, true)
	if err != nil {
		return nil, err
	}
	req.NodeUUID = nodes[0].UUID
	req.NNodes, req.Replicas = 1, 1
	return &pdRequest{pd: pd, mode: mode, nodes: nodes, warnings: warnings, sleepable: req.Sleepable}, nil
}

// pdGroup is the network a PD request's KV transfer would use, refused when
// its nodes share none or the one they share cannot carry the mode's
// transport. Creating and previewing a deployment both ask it, as the
// controller does again at every launch.
func (a *apiHandlers) pdGroup(p *pdRequest) (netplan.Group, error) {
	plain := make([]store.Node, len(p.nodes))
	for i, n := range p.nodes {
		plain[i] = *n
	}
	resolved, groupErr := a.networks().ComputeGroup(plain)
	group, err := planning.PDGroup(plain, resolved, groupErr)
	if err != nil {
		if _, mapped := a.computeGroup(p.nodes); mapped != nil {
			err = mapped
		}
		return group, err
	}
	return group, planning.CheckPDNetwork(p.mode, plain, group, p.sleepable)
}

// checkEngine rejects a PD request whose engine, settled only after the
// template applied, is not the mode's.
func (p *pdRequest) checkEngine(engine string) error {
	if p == nil {
		return nil
	}
	_, err := planning.ValidatePD(p.pd, engine)
	return err
}

// preflight is the decode role's side of the create preflight: the decode TP
// and a PD launch, so the check sees the connector the instances will run.
func (p *pdRequest) preflight(params *deploy.Params) {
	if p == nil {
		return
	}
	tp := p.pd.TP(pdmode.RoleDecode)
	params.TensorParallel, params.AssignedGPUs = tp, tp
	params.PD = &deploy.PDLaunch{Mode: p.mode.Name, Role: pdmode.RoleDecode, KVLoadFailure: p.pd.EffectiveKVLoadFailure()}
}

// otherNodes are the PD nodes after the primary one, which the create path
// admits like a multi-node group's other ranks.
func (p *pdRequest) otherNodes() []*store.Node {
	if p == nil || len(p.nodes) < 2 {
		return nil
	}
	return p.nodes[1:]
}

// deploymentPDDTO is the PD section of a deployment response. Its role counts
// are the ready and asleep counts of the same walk the response's own
// ready_replicas and asleep_replicas come from (deploymentFactsOf), so a role
// never reads ready in one field and asleep in another, and the sleep state of
// the deployment is read against the roles the gateway needs (a prefill role
// its mode serves without does not make the deployment asleep when empty).
func deploymentPDDTO(rows dtoLookup, d *store.Deployment, insts []store.Instance, view replicaView, facts deploymentFacts) *dto.DeploymentPD {
	pd, err := planning.PDOf(d.Placement)
	if err != nil || pd == nil {
		return nil
	}
	names, match := view.names, view.match
	mode, _ := pdmode.Lookup(pd.Mode)
	slots := map[string]planning.UnitSlot{}
	for _, slot := range match.Slots {
		slots[fmt.Sprintf("%s/%d", slot.Unit.Role, slot.Unit.Index)] = slot
	}
	out := &dto.DeploymentPD{Mode: pd.Mode, Fallback: pd.EffectiveFallback(), KVLoadFailure: pd.EffectiveKVLoadFailure(),
		State: pdStateOf(view, facts), Roles: []dto.DeploymentPDRole{}}
	for _, role := range view.roles() {
		counts := facts.Roles[role]
		unitCount := len(pd.Units(role))
		row := dto.DeploymentPDRole{Role: role, TP: pd.TP(role), PP: pd.Roles[role].PP, Desired: unitCount, Ready: counts.Awake,
			Asleep: counts.Asleep, MayBeEmpty: mode.MayBeEmpty(role), Units: []dto.DeploymentPDUnit{}}
		if row.Ready < row.Desired {
			out.Short = true
		}
		for index, unit := range pd.Units(role) {
			item := dto.DeploymentPDUnit{NodeUUID: unit.NodeUUID, NodeName: names[unit.NodeUUID], GPUs: unit.GPUIndexes}
			if slot, ok := slots[fmt.Sprintf("%s/%d", role, index)]; ok {
				unitAttempts(rows, &item, slot, match.Failed[slot.Key])
			}
			for _, rank := range unit.Followers {
				item.Followers = append(item.Followers, dto.DeploymentPDRank{NodeUUID: rank.NodeUUID, NodeName: names[rank.NodeUUID], GPUs: rank.GPUIndexes})
			}
			row.Units = append(row.Units, item)
		}
		out.Roles = append(out.Roles, row)
	}
	return out
}

// pdStateOf is a PD deployment's state from the gateway's own reading of its
// roles, not from a separate count: running when every role the gateway needs
// has an awake replica, partial when one of them has none, not_ready when
// nothing is awake and the roles are asleep (or absent), and the same states
// distinguish a role asleep from a role down the way the deployment's own
// state does.
func pdStateOf(view replicaView, facts deploymentFacts) string {
	anyAwake := false
	for _, role := range view.roles() {
		if facts.Roles[role].Awake > 0 {
			anyAwake = true
		}
	}
	if !anyAwake {
		return planning.PDStateNotReady
	}
	for _, role := range view.roles() {
		counts := facts.Roles[role]
		if counts.Awake > 0 || !view.roleRequired(role, counts) {
			continue
		}
		return planning.PDStatePartial
	}
	return planning.PDStateRunning
}

// unitAttempts fills a unit's spent retry budget: how many launches failed,
// the newest failure, and whether the controller gave up on it.
func unitAttempts(rows dtoLookup, item *dto.DeploymentPDUnit, slot planning.UnitSlot, attempts []store.Instance) {
	spent := spentAttempts(rows, slot, attempts)
	item.FailedAttempts, item.LastFailure, item.RetryExhausted = spent.count, spent.lastFailure, spent.exhausted
}

// evaluatePDEnv is what the PD preview needs from the evaluate handler.
type evaluatePDEnv struct {
	ctx               context.Context
	writer            http.ResponseWriter
	request           *dto.DeployRequest
	excludeDeployment uint
	response          *dto.EvaluateResponse
	engine            deploy.Engine
	model             *store.Model
	selection         templateSelection
	launch            deploy.Materialized
	requestOverrides  deploy.ContainerOverrides
	servedName        string
	errs              *evaluateFieldErrors
	// targetVendor and profileNode feed the image and acquisition facts, as
	// on the ordinary path; the profile node is the placement's first node.
	targetVendor string
	profileNode  *store.Node
}

// evaluatePD previews a PD draft: every unit of every role is planned on its
// node with its role's TP and configuration, and gets its own command, so the
// form shows what each instance would run and whether it fits. The top-level
// command fields describe the first decode instance.
func (a *apiHandlers) evaluatePD(env evaluatePDEnv) {
	w, req, resp := env.writer, env.request, env.response
	prepared, err := a.preparePDDeployRequest(req, env.engine.Name())
	if err == nil {
		err = planning.CheckPDArgs(env.engine.Name(), env.launch.TemplateResidual, env.launch.UserResidual)
	}
	if err != nil {
		env.errs.add(fieldPDPlacement, err)
		writeEvaluateFieldErrors(w, resp, env.errs)
		return
	}
	pd, mode := prepared.pd, prepared.mode
	resp.Warnings = append(resp.Warnings, prepared.warnings...)
	// The image and where it and the weights come from are the same questions
	// as for any deployment, answered for every node the placement names: the
	// launches below run the resolved image.
	resp.WeightSources = a.evaluateWeightSourceOptions(env.selection)
	a.evaluateImageFacts(evaluateImageEnv{
		ctx: env.ctx, request: req, response: resp, engine: env.engine, selection: env.selection,
		targetVendor: env.targetVendor, profileNode: env.profileNode, node: prepared.nodes[0], nodeFound: true,
		weightFacts: a.evaluateWeightFacts(env.model, env.selection, env.profileNode),
	})
	if len(prepared.nodes) > 1 && resp.Acquisition != nil {
		uuids := make([]string, len(prepared.nodes))
		for i, node := range prepared.nodes {
			uuids[i] = node.UUID
		}
		resp.Acquisition.Nodes = a.evaluateNodeAcquisition(env.ctx, uuids, resp.Image, templateImageSources(env.selection, resp.Image), env.model, store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: a.previewSaveToStorageID(req.ModelSource, env.model, prepared.nodes[0])}, nil)
	}
	q := env.launch.Quick
	resp.Warnings = append(resp.Warnings, planning.PDFeatureWarnings(q.KVOffloadingSize > 0 || q.KVDiskOffload, req.VRAMLimitGiB > 0)...)
	if mode.Unverified {
		resp.Warnings = append(resp.Warnings, messages.Warning{Code: messages.WarningPDUnverified, Params: map[string]any{"feature": "mode", "mode": mode.Name}})
	}
	group, err := a.pdGroup(prepared)
	if err != nil {
		env.errs.add(fieldPDPlacement, err)
		writeEvaluateFieldErrors(w, resp, env.errs)
		return
	}
	resp.Warnings = append(resp.Warnings, computeFallbackWarning(group)...)
	byUUID := map[string]*store.Node{}
	for _, n := range prepared.nodes {
		byUUID[n.UUID] = n
	}
	resp.PD = &dto.EvaluatePD{Mode: mode.Name, Fallback: pd.EffectiveFallback(), KVLoadFailure: pd.EffectiveKVLoadFailure()}
	for _, role := range mode.Roles {
		row := dto.EvaluatePDRole{Role: role, TP: pd.TP(role), PP: pd.Roles[role].PP}
		for _, unit := range pd.Units(role) {
			ranks := unit.Ranks()
			var item dto.EvaluatePDUnit
			leaderAddress := ""
			for index, rank := range ranks {
				node := byUUID[rank.NodeUUID]
				compute := group.Ranks[node.ID]
				topology, err := planning.RankTopology(node, &compute, planning.GroupRank{Ranks: len(ranks), Rank: index})
				if err != nil {
					env.errs.add(fieldPDPlacement, err)
					writeEvaluateFieldErrors(w, resp, env.errs)
					return
				}
				if index == 0 {
					leaderAddress = topology.NodeAddress
				}
				preview, plan, err := a.previewPDRank(env, pd, mode, role, unit, index, topology, leaderAddress)
				if err != nil {
					env.errs.add(fieldPDPlacement, err)
					writeEvaluateFieldErrors(w, resp, env.errs)
					return
				}
				resp.Warnings = append(resp.Warnings, plan.Warnings...)
				resp.Warnings = append(resp.Warnings, plan.Output.Warnings...)
				if !preview.Fits {
					resp.OK = false
				}
				if index > 0 {
					item.Followers = append(item.Followers, preview)
					continue
				}
				item = preview
				if resp.Command == nil {
					setEvaluateLaunch(resp, plan.Resolved)
					resp.Breakdown = plan.Output.Breakdown
				}
			}
			row.Units = append(row.Units, item)
		}
		resp.PD.Roles = append(resp.PD.Roles, row)
	}
	resp.Replicas = pd.InstanceCount()
	resp.PlanScope = "per_instance"
	if !env.errs.empty() {
		resp.OK = false
		resp.Errors = env.errs.items
	}
	resp.Warnings = uniqueWarnings(resp.Warnings)
	writeJSON(w, http.StatusOK, *resp)
}

// previewPDRank plans one rank of a PD unit on its node, as the controller
// would launch it: the role's parallelism over the unit's ranks, the role's
// configuration, and a multi-node rank's meeting point at the leader's
// address on the default collective port.
func (a *apiHandlers) previewPDRank(env evaluatePDEnv, pd *planning.PDPlacement, mode pdmode.Mode, role string, unit planning.PDUnit, rankIndex int, topology planning.DistTopology, leaderAddress string) (dto.EvaluatePDUnit, planning.LaunchPlan, error) {
	req := env.request
	target, err := a.d.Store.NodeWithGPUs(unit.Ranks()[rankIndex].NodeUUID)
	if err != nil {
		return dto.EvaluatePDUnit{}, planning.LaunchPlan{}, err
	}
	rankSelection := snapshotTemplateSelection(env.selection, env.response.Image)
	rankSelection.ExtraArgs = env.launch.TemplateResidual
	ports := planning.PDRankPorts{SideChannel: planning.PreviewAPIPort + 1, LeaderAddress: leaderAddress}
	if unit.MultiNode() {
		ports.Master = deploy.DefaultMasterPort
	}
	modelMount, componentMounts := previewComponentMounts(a.d.Store, env.selection.Components, target)
	launch := planning.LaunchRequest{
		Preview: true, ExcludeDeploymentID: env.excludeDeployment, Engine: env.engine.Name(), Model: env.model, Node: target,
		Selection: rankSelection, Quick: env.launch.Quick, ExtraArgs: env.launch.UserResidual,
		Source:     planning.LaunchSourceFromRequest(env.response.Image, env.requestOverrides),
		ModelMount: modelMount, ComponentMounts: componentMounts,
		ServedName: env.servedName, Sleepable: req.Sleepable, YieldPolicy: requestYield(req.YieldPolicy),
		VRAMLimitBytes: int64(req.VRAMLimitGiB * (1 << 30)), PartitionBackend: req.PartitionBackend,
		APIContract: rankSelection.APIContract,
		ModelRules:  store.SourceRules{Pinned: store.StoragePins(req.NodeRequirements), SaveTo: a.previewSaveToStorageID(req.ModelSource, env.model, target)},
	}
	pd.RankLaunch(mode, role, unit, rankIndex, topology, ports).Apply(&launch)
	plan, err := planning.PlanLaunch(env.ctx, a.d.Store, a.networks(), launch)
	if err != nil {
		return dto.EvaluatePDUnit{}, plan, err
	}
	res := plan.Resolved
	fits := plan.Output.Fits()
	item := dto.EvaluatePDUnit{
		NodeUUID: target.UUID, NodeName: nodeDisplayName(target), Address: topology.NodeAddress,
		Fits: fits, Breakdown: plan.Output.Breakdown,
		Entrypoint: res.Entrypoint, Argv: res.Argv, Command: launchCommand(res),
		Env: res.Env, DockerArgs: res.DockerArgs, ShmBytes: res.ShmBytes, Mounts: evaluateMounts(res.Mounts),
	}
	if fits {
		item.GPUs = plan.Output.Result.Plan.GPUIndexes
	} else {
		item.Reason = plan.Output.Refusal(target, servicelevel.Policy{Yield: requestYield(req.YieldPolicy)})
	}
	return item, plan, nil
}

// checkPDSpec enforces a PD-only catalog spec (plan D15): it deploys only as
// PD, in the mode it was written for. An ordinary spec may deploy either way.
func checkPDSpec(selection templateSelection, pd *planning.PDPlacement) error {
	if selection.Spec == nil || selection.Spec.PDMode == "" {
		return nil
	}
	if pd == nil || pd.Mode != selection.Spec.PDMode {
		requested := ""
		if pd != nil {
			requested = pd.Mode
		}
		return productError(messages.ErrorPDSpecModeMismatch, "spec "+selection.Spec.Name+" is deployed only as PD in mode "+selection.Spec.PDMode, map[string]any{"spec": selection.Spec.Name, "mode": selection.Spec.PDMode, "requested": requested})
	}
	return nil
}
