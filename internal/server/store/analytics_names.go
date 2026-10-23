package store

// Names for operations analytics: the deployments, nodes and users its rows
// refer to, read for those IDs only, lean columns, and deployments deleted
// since from what they left behind.

// DeploymentName is what a statistic shows for a deployment.
type DeploymentName struct {
	Name       string
	ServedName string
	Deleted    bool
}

// DeploymentNames names the deployments of ids, live or deleted; an ID
// neither knows is left out.
func (s *Store) DeploymentNames(ids []uint) (map[uint]DeploymentName, error) {
	out := make(map[uint]DeploymentName, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var live []Deployment
	if err := s.reader().Select("ID", "Name", "ServedName").Where("id IN ?", ids).Find(&live).Error; err != nil {
		return nil, err
	}
	for _, d := range live {
		out[d.ID] = DeploymentName{Name: d.Name, ServedName: d.ServedName}
	}
	var missing []uint
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			missing = append(missing, id)
		}
	}
	gone, err := s.DeletedDeployments(missing)
	if err != nil {
		return nil, err
	}
	for id, d := range gone {
		out[id] = DeploymentName{Name: d.Name, ServedName: d.ServedName, Deleted: true}
	}
	return out, nil
}

// NodeNamesByUUID names the nodes of uuids: the name given, else the host's.
func (s *Store) NodeNamesByUUID(uuids []string) (map[string]string, error) {
	out := make(map[string]string, len(uuids))
	if len(uuids) == 0 {
		return out, nil
	}
	var nodes []Node
	if err := s.reader().Select("UUID", "Name", "Hostname").Where("uuid IN ?", uuids).Find(&nodes).Error; err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.Name != "" {
			out[n.UUID] = n.Name
		} else {
			out[n.UUID] = n.Hostname
		}
	}
	return out, nil
}

// UserNames names the users of ids.
func (s *Store) UserNames(ids []uint) (map[uint]string, error) {
	out := make(map[uint]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var users []User
	if err := s.reader().Select("ID", "Username").Where("id IN ?", ids).Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.ID] = u.Username
	}
	return out, nil
}
