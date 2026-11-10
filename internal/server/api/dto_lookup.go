package api

import (
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// dtoLookup resolves the rows a deployment or instance DTO joins in. A missing
// row is nil: the DTO simply leaves that field empty, exactly as it does when
// the store reports not-found.
type dtoLookup interface {
	cachePhase(store.FileRef) string
	preparationNodes(*store.Deployment) []uint
	modelSource(uint, *store.Node, store.SourceRules) store.ModelSource
	modelCopyObservation(*store.ModelFile) (proto.ModelFileStatus, bool)
	instances(deploymentID uint) []store.Instance
	deployment(id uint) *store.Deployment
	node(id uint) *store.Node
	model(id uint) *store.Model
	modelFile(modelID, nodeID uint) *store.ModelFile
	firstScheduleEvent(deploymentID uint) (store.InstanceEvent, bool)
	latestScheduleEvent(deploymentID uint) (store.InstanceEvent, bool)
	// retryExhausted reports whether the controller recorded that the unit
	// whose newest failed attempt this is gave up (ADR 0029).
	retryExhausted(instanceUUID string) bool
	pendingTimeout() time.Duration
	// requirements is the deployment's node requirements; nil when it names
	// none or they cannot be read.
	requirements(deploymentID uint) map[string][]string
	// modelReads are the model files the instance reads.
	modelReads(instanceID uint) []dto.InstanceModelFile
	// nodeByUUID is the node with that UUID; nil when there is none.
	nodeByUUID(uuid string) *store.Node
}

// storeLookup asks the store for each row as it is needed. It suits a single
// object; over a list it costs several queries per row.
type storeLookup struct {
	st *store.Store
	preparationReads
}

func (l storeLookup) instances(deploymentID uint) []store.Instance {
	insts, _ := l.st.InstancesForDeployment(deploymentID)
	return insts
}

func (l storeLookup) deployment(id uint) *store.Deployment {
	d, err := l.st.GetDeployment(id)
	if err != nil {
		return nil
	}
	return d
}

func (l storeLookup) node(id uint) *store.Node {
	n, err := l.st.NodeByID(id)
	if err != nil {
		return nil
	}
	return n
}

func (l storeLookup) model(id uint) *store.Model {
	m, err := l.st.GetModel(id)
	if err != nil {
		return nil
	}
	return m
}

func (l storeLookup) modelFile(modelID, nodeID uint) *store.ModelFile {
	mf, err := l.st.ModelFileByModelNode(modelID, nodeID)
	if err != nil {
		return nil
	}
	return mf
}

func (l storeLookup) firstScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found, err := l.st.FirstScheduleEvent(deploymentID)
	return ev, found && err == nil
}

func (l storeLookup) retryExhausted(instanceUUID string) bool {
	recorded, err := l.st.InstancesWithEvent(store.EventUnitRetryExhausted, []string{instanceUUID})
	return err == nil && recorded[instanceUUID]
}

func (l storeLookup) latestScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found, err := l.st.LatestScheduleEvent(deploymentID)
	return ev, found && err == nil
}

func (l storeLookup) pendingTimeout() time.Duration {
	return l.st.DeploymentPolicy().PendingTimeout()
}

func (l storeLookup) requirements(deploymentID uint) map[string][]string {
	reqs, _ := l.st.DeploymentRequirements(deploymentID)
	return reqs
}

func (l storeLookup) modelReads(instanceID uint) []dto.InstanceModelFile {
	reads, err := l.st.InstanceModelFiles([]uint{instanceID})
	if err != nil {
		return nil
	}
	return instanceReadDTOs(reads[instanceID], func(id uint) *store.ModelFile {
		mf, err := l.st.ModelFileByID(id)
		if err != nil {
			return nil
		}
		return mf
	}, func(id uint) *store.Storage {
		st, err := l.st.StorageByID(id)
		if err != nil {
			return nil
		}
		return st
	})
}

func (l storeLookup) nodeByUUID(uuid string) *store.Node {
	n, err := l.st.NodeByUUID(uuid)
	if err != nil {
		return nil
	}
	return n
}

// instanceReadDTOs describes an instance's recorded reads with the storage
// each file is in.
func instanceReadDTOs(reads []store.InstanceModelFile, file func(uint) *store.ModelFile, storage func(uint) *store.Storage) []dto.InstanceModelFile {
	if len(reads) == 0 {
		return nil
	}
	out := make([]dto.InstanceModelFile, 0, len(reads))
	for _, read := range reads {
		item := dto.InstanceModelFile{ModelID: read.ModelID, Path: read.HostPath}
		if mf := file(read.ModelFileID); mf != nil {
			if st := storage(mf.StorageID); st != nil {
				item.StorageID, item.StorageName, item.StorageType = st.ID, st.Name, st.Type
			}
		}
		out = append(out, item)
	}
	return out
}

type modelNodeKey struct{ modelID, nodeID uint }

// listLookup holds everything a whole list joins against, read once. The query
// count is then independent of how many rows the list has.
type listLookup struct {
	storageRows           []store.Storage
	sources               *store.ModelSourceSnapshot
	instancesByDeployment map[uint][]store.Instance
	deployments           map[uint]*store.Deployment
	nodes                 map[uint]*store.Node
	models                map[uint]*store.Model
	modelFiles            map[modelNodeKey]*store.ModelFile
	scheduleEvents        map[uint]store.InstanceEvent
	// gaveUp is which failed instances carry the controller's give-up
	// record, read once for the whole list.
	gaveUp  map[string]bool
	reqs    map[uint]map[string][]string
	reads   map[uint][]dto.InstanceModelFile
	byUUID  map[string]*store.Node
	latest  map[uint]store.InstanceEvent
	timeout time.Duration
	// st is the store the list was read from.
	st *store.Store
	preparationReads
}

func newListLookup(st *store.Store, prep ModelPreparation, deployments []store.Deployment, instances []store.Instance) (*listLookup, error) {
	l := &listLookup{
		st:                    st,
		preparationReads:      preparationReads{prep},
		instancesByDeployment: map[uint][]store.Instance{},
		deployments:           make(map[uint]*store.Deployment, len(deployments)),
		nodes:                 map[uint]*store.Node{},
		models:                map[uint]*store.Model{},
		modelFiles:            map[modelNodeKey]*store.ModelFile{},
		timeout:               st.DeploymentPolicy().PendingTimeout(),
	}
	for i := range deployments {
		l.deployments[deployments[i].ID] = &deployments[i]
	}
	// ListInstances and InstancesForDeployment share an order, so grouping
	// keeps each deployment's instances in the order the single path returns.
	for _, inst := range instances {
		l.instancesByDeployment[inst.DeploymentID] = append(l.instancesByDeployment[inst.DeploymentID], inst)
	}
	nodes, err := st.NodeIdentities()
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		l.nodes[nodes[i].ID] = &nodes[i]
	}
	models, err := st.ListModels()
	if err != nil {
		return nil, err
	}
	for i := range models {
		l.models[models[i].ID] = &models[i]
	}
	modelFiles, err := st.ListModelFiles()
	if err != nil {
		return nil, err
	}
	fileByID := make(map[uint]*store.ModelFile, len(modelFiles))
	for i := range modelFiles {
		fileByID[modelFiles[i].ID] = &modelFiles[i]
		key := modelNodeKey{modelFiles[i].ModelID, modelFiles[i].NodeID}
		if _, seen := l.modelFiles[key]; !seen {
			l.modelFiles[key] = &modelFiles[i]
		}
	}
	if err := l.loadReads(st, instances, fileByID); err != nil {
		return nil, err
	}
	if l.scheduleEvents, err = st.FirstScheduleEvents(); err != nil {
		return nil, err
	}
	var failed []string
	for _, inst := range instances {
		if inst.State == proto.InstFailed {
			failed = append(failed, inst.UUID)
		}
	}
	if l.gaveUp, err = st.InstancesWithEvent(store.EventUnitRetryExhausted, failed); err != nil {
		return nil, err
	}
	ids := make([]uint, 0, len(deployments))
	for i := range deployments {
		ids = append(ids, deployments[i].ID)
	}
	if l.latest, err = st.LatestScheduleEvents(); err != nil {
		return nil, err
	}
	all, err := st.DeploymentRequirementsFor(ids)
	if err != nil {
		return nil, err
	}
	l.reqs = make(map[uint]map[string][]string, len(all))
	for id, reqs := range all {
		l.reqs[id] = reqs
	}
	if err := l.loadPreparationSources(deployments, modelFiles); err != nil {
		return nil, err
	}
	return l, nil
}

// loadReads reads every listed instance's model files once.
func (l *listLookup) loadReads(st *store.Store, instances []store.Instance, fileByID map[uint]*store.ModelFile) error {
	ids := make([]uint, 0, len(instances))
	for _, inst := range instances {
		ids = append(ids, inst.ID)
	}
	reads, err := st.InstanceModelFiles(ids)
	if err != nil {
		return err
	}
	storages, err := st.ListStorages()
	if err != nil {
		return err
	}
	l.storageRows = storages
	storageByID := make(map[uint]*store.Storage, len(storages))
	for i := range storages {
		storageByID[storages[i].ID] = &storages[i]
	}
	l.reads = make(map[uint][]dto.InstanceModelFile, len(reads))
	for id, rows := range reads {
		l.reads[id] = instanceReadDTOs(rows, func(id uint) *store.ModelFile { return fileByID[id] }, func(id uint) *store.Storage { return storageByID[id] })
	}
	return nil
}

func (l *listLookup) modelReads(instanceID uint) []dto.InstanceModelFile { return l.reads[instanceID] }

func (l *listLookup) nodeByUUID(uuid string) *store.Node {
	if l.byUUID == nil {
		l.byUUID = make(map[string]*store.Node, len(l.nodes))
		for _, node := range l.nodes {
			l.byUUID[node.UUID] = node
		}
	}
	return l.byUUID[uuid]
}

func (l *listLookup) instances(deploymentID uint) []store.Instance {
	return l.instancesByDeployment[deploymentID]
}
func (l *listLookup) deployment(id uint) *store.Deployment { return l.deployments[id] }
func (l *listLookup) node(id uint) *store.Node             { return l.nodes[id] }
func (l *listLookup) model(id uint) *store.Model           { return l.models[id] }
func (l *listLookup) modelFile(modelID, nodeID uint) *store.ModelFile {
	return l.modelFiles[modelNodeKey{modelID, nodeID}]
}
func (l *listLookup) firstScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found := l.scheduleEvents[deploymentID]
	return ev, found
}
func (l *listLookup) pendingTimeout() time.Duration { return l.timeout }
func (l *listLookup) latestScheduleEvent(deploymentID uint) (store.InstanceEvent, bool) {
	ev, found := l.latest[deploymentID]
	return ev, found
}
func (l *listLookup) retryExhausted(instanceUUID string) bool { return l.gaveUp[instanceUUID] }
func (l *listLookup) requirements(deploymentID uint) map[string][]string {
	return l.reqs[deploymentID]
}
