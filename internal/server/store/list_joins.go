package store

// RoutingDeployments returns the active deployment rows needed by the proxy's
// in-memory routing table. It deliberately avoids joins and preloads.
func (s *Store) RoutingDeployments() ([]Deployment, error) {
	var rows []Deployment
	err := s.reader().Where("desired_state = ?", string(DeployActive)).Order("id desc").Find(&rows).Error
	return rows, err
}

// RoutingInstances returns running instance rows for the proxy route table.
func (s *Store) RoutingInstances() ([]Instance, error) {
	var rows []Instance
	err := s.reader().Where("state = ?", "running").Order("id desc").Find(&rows).Error
	return rows, err
}

// NodeUUIDsByID returns the small node identity map used by proxy routing.
func (s *Store) NodeUUIDsByID() (map[uint]string, error) {
	var rows []Node
	if err := s.reader().Select("id", "uuid").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]string, len(rows))
	for _, row := range rows {
		out[row.ID] = row.UUID
	}
	return out, nil
}

// The list endpoints join every row against nodes, models, model files and
// schedule events. These readers fetch each of those once for the whole list,
// so the query count stays constant as the fleet grows.

// NodeIdentities returns only what is needed to name a node. GPUs and
// credential flags are left out: a list of a thousand nodes would otherwise
// load every GPU row to print a label.
func (s *Store) NodeIdentities() ([]Node, error) {
	var nodes []Node
	err := s.db.Select("id", "uuid", "name", "hostname", "state", "labels").Find(&nodes).Error
	return nodes, err
}

// ListModelFiles returns every model file, oldest first, so the first row seen
// for a (model, node) pair is the one ModelFileByModelNode returns.
func (s *Store) ListModelFiles() ([]ModelFile, error) {
	var mfs []ModelFile
	err := s.db.Order("id asc").Find(&mfs).Error
	return mfs, err
}

// FirstScheduleEvents returns each deployment's earliest scheduling event, the
// same row FirstScheduleEvent returns for one deployment.
func (s *Store) FirstScheduleEvents() (map[uint]InstanceEvent, error) {
	var events []InstanceEvent
	firsts := s.db.Model(&InstanceEvent{}).Select("MIN(id)").
		Where("kind = ? AND deployment_id <> 0", EventKindSchedule).Group("deployment_id")
	if err := s.db.Where("id IN (?)", firsts).Find(&events).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]InstanceEvent, len(events))
	for _, ev := range events {
		out[ev.DeploymentID] = ev
	}
	return out, nil
}

// LatestScheduleEvents is every deployment's newest scheduling event, read in
// one query for a list: what the deployment's placement status shows.
func (s *Store) LatestScheduleEvents() (map[uint]InstanceEvent, error) {
	var events []InstanceEvent
	latest := s.db.Model(&InstanceEvent{}).Select("MAX(id)").
		Where("kind = ? AND deployment_id <> 0", EventKindSchedule).Group("deployment_id")
	if err := s.db.Where("id IN (?)", latest).Find(&events).Error; err != nil {
		return nil, err
	}
	out := make(map[uint]InstanceEvent, len(events))
	for _, ev := range events {
		out[ev.DeploymentID] = ev
	}
	return out, nil
}
