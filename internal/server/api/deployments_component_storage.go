package api

import (
	"errors"
	"net/http"
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// templateComponent is one of a template's non-primary components with its
// model row.
type templateComponent struct {
	binding deploy.ComponentBinding
	model   *store.Model
}

// templateComponents are a template's non-primary components, in declaration
// order. The primary is the deployment's own model and goes through the
// create's model checks already.
func (a *apiHandlers) templateComponents(components []deploy.ComponentBinding) ([]templateComponent, error) {
	var out []templateComponent
	for _, component := range components {
		if component.Role == "primary" || component.StoreModelID == 0 {
			continue
		}
		model, err := a.d.Store.GetModel(component.StoreModelID)
		if err != nil {
			return nil, err
		}
		out = append(out, templateComponent{binding: component, model: model})
	}
	return out, nil
}

// Component acquisition plans, as dto.AcquisitionComponent.Plan carries them.
const (
	componentPlanReady          = "ready"
	componentPlanSharedDownload = "shared_download"
	componentPlanNodeDownload   = "node_download"
)

// componentPlan is how a template component reaches the target nodes. The
// evaluate forecast shows it and the create acts on it, so what the form
// says is what the create does.
type componentPlan struct {
	templateComponent
	plan       string
	nodes      int
	readyNodes int
	// storage is the shared storage the create downloads the component into,
	// once through a writer node, for shared_download.
	storage *store.Storage
	// followsPrimary says the request named no storage and storage is the
	// shared storage the primary sits on (ADR 0062).
	followsPrimary bool
}

// planComponents decides, for each template component, how the target nodes
// get it:
//
//   - ready when every target reads it now;
//   - shared_download into saveTo when the request named a shared storage;
//   - with no storage named, shared_download into the shared storage the
//     primary sits on, when that storage serves every target, accepts new
//     files and no write of the component into it is under way already (the
//     nodes wait only for a write their deployment starts, see
//     store.ModelSourceOn), so a component no target reads is fetched once,
//     next to the primary, rather than by each node into local storage;
//   - else node_download: each target without it fetches it itself.
//
// The deployment still keeps only the storage the request named.
func (a *apiHandlers) planComponents(source *dto.ModelSource, primary *store.Model, components []templateComponent, saveTo *store.Storage, pinned []uint, targets []*store.Node) ([]componentPlan, error) {
	nodes := uniqueNodes(targets)
	if len(components) == 0 || len(nodes) == 0 {
		return nil, nil
	}
	named := source != nil && source.StorageID != 0
	var beside []*store.Storage
	besideKnown := false
	out := make([]componentPlan, 0, len(components))
	for _, component := range components {
		plan := componentPlan{templateComponent: component, nodes: len(nodes)}
		for _, node := range nodes {
			src, err := a.d.Store.ModelSourceOn(component.model.ID, node, store.SourceRules{Pinned: pinned})
			if err != nil {
				return nil, err
			}
			if src.Action == store.SourceReady {
				plan.readyNodes++
			}
		}
		switch {
		case plan.readyNodes == plan.nodes:
			plan.plan = componentPlanReady
		case named && saveTo != nil && proto.StorageShared(saveTo.Type):
			plan.plan, plan.storage = componentPlanSharedDownload, saveTo
		case named:
			plan.plan = componentPlanNodeDownload
		default:
			if !besideKnown {
				var err error
				if beside, err = a.primarySharedStorages(primary, pinned, nodes); err != nil {
					return nil, err
				}
				besideKnown = true
			}
			plan.plan = componentPlanNodeDownload
			for _, storage := range beside {
				if busy, err := a.sharedWriteUnderWay(component.model, storage); err != nil {
					return nil, err
				} else if !busy {
					plan.plan, plan.storage, plan.followsPrimary = componentPlanSharedDownload, storage, true
					break
				}
			}
		}
		out = append(out, plan)
	}
	return out, nil
}

// primarySharedStorages are the shared storages that hold the primary model
// for every target, serve every target and accept new files: the default
// storage first, then storage id order, as in ModelSourceOn.
func (a *apiHandlers) primarySharedStorages(primary *store.Model, pinned []uint, nodes []*store.Node) ([]*store.Storage, error) {
	if primary == nil {
		return nil, nil
	}
	storages, err := a.d.Store.ListStorages()
	if err != nil {
		return nil, err
	}
	allowed := map[uint]bool{}
	for _, id := range pinned {
		allowed[id] = true
	}
	sort.SliceStable(storages, func(i, j int) bool {
		if storages[i].IsDefault != storages[j].IsDefault {
			return storages[i].IsDefault
		}
		return storages[i].ID < storages[j].ID
	})
	var out []*store.Storage
	for i := range storages {
		storage := &storages[i]
		if !proto.StorageShared(storage.Type) || (len(allowed) > 0 && !allowed[storage.ID]) {
			continue
		}
		if a.d.Store.StorageAcceptsWrites(storage) != nil {
			continue
		}
		servesAll := true
		for _, node := range nodes {
			if _, applies, err := a.d.Store.EffectiveStorageForNode(storage, node); err != nil {
				return nil, err
			} else if !applies {
				servesAll = false
				break
			}
		}
		if servesAll && a.storageHoldsModel(storage, primary, nodes) {
			out = append(out, storage)
		}
	}
	return out, nil
}

// sharedWriteUnderWay says a write of the model into the storage is under
// way already, started by someone else.
func (a *apiHandlers) sharedWriteUnderWay(model *store.Model, storage *store.Storage) (bool, error) {
	row, err := a.d.Store.ModelFileAt(store.FileRef{ModelID: model.ID, StorageID: storage.ID})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return store.TransferUnderWay(row), nil
}

// acquisitionComponents is the wire form of the plans.
func acquisitionComponents(plans []componentPlan) []dto.AcquisitionComponent {
	out := make([]dto.AcquisitionComponent, 0, len(plans))
	for _, plan := range plans {
		size := plan.binding.WeightBytes
		if size == 0 {
			size = plan.model.WeightBytes
		}
		item := dto.AcquisitionComponent{
			Name: plan.binding.Name, Role: plan.binding.Role, Model: plan.model.DisplayName(), SizeBytes: size,
			Plan: plan.plan, Nodes: plan.nodes, ReadyNodes: plan.readyNodes, FollowsPrimary: plan.followsPrimary,
		}
		if plan.storage != nil {
			item.StorageID, item.Storage = plan.storage.ID, plan.storage.Name
		}
		out = append(out, item)
	}
	return out
}

// startComponentDownloads starts, for each component planned into a shared
// storage that does not hold it yet, the one write through a writer node, owned
// by the deployment, and records on its timeline where the component goes and
// which node writes it, so the write into shared storage is never silent.
func (a *apiHandlers) startComponentDownloads(plans []componentPlan, targets []*store.Node, deploymentID uint) (int, error) {
	nodes := uniqueNodes(targets)
	for _, plan := range plans {
		if plan.plan != componentPlanSharedDownload || plan.storage == nil || a.storageHoldsModel(plan.storage, plan.model, nodes) {
			continue
		}
		writer, status, err := a.startSharedDownloadOn(plan.model, plan.storage, "", deploymentID)
		if err != nil {
			return status, err
		}
		if writer == nil {
			continue
		}
		a.d.Store.AddDeploymentEventCode(deploymentID, store.EventKindLifecycle, store.EventDeploymentComponentDownload, map[string]any{
			"component": plan.binding.Name, "model": plan.model.DisplayName(), "bytes": acquisitionComponents([]componentPlan{plan})[0].SizeBytes,
			"storage": plan.storage.Name, "node": writer.DisplayName(), "follows_primary": plan.followsPrimary,
		})
	}
	return http.StatusOK, nil
}

// evaluateComponentAcquisition is the evaluate forecast of planComponents for
// the nodes the draft lands on; empty until they are known, and when the
// request names a storage the create would refuse (the form reports that
// storage on its own).
func (a *apiHandlers) evaluateComponentAcquisition(source *dto.ModelSource, primary *store.Model, bindings []deploy.ComponentBinding, pinned []uint, targets []*store.Node) []dto.AcquisitionComponent {
	if len(targets) == 0 {
		return nil
	}
	components, err := a.templateComponents(bindings)
	if err != nil || len(components) == 0 {
		return nil
	}
	saveTo, err := a.resolveSaveToStorage(source, primary, targets)
	if err != nil {
		return nil
	}
	plans, err := a.planComponents(source, primary, components, saveTo, pinned, targets)
	if err != nil {
		return nil
	}
	return acquisitionComponents(plans)
}
