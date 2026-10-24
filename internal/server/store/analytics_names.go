package store

// Names for usage analytics: the deployments, nodes and users its rows
// refer to, read for those IDs only, lean columns, and deployments and nodes
// deleted since from what they left behind.

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

// NodeName is what a statistic shows for a node.
type NodeName struct {
	Name    string
	Deleted bool
}

// NodeNamesByUUID names the nodes of uuids, live or deleted: the name given,
// else the host's; a UUID neither knows is left out.
func (s *Store) NodeNamesByUUID(uuids []string) (map[string]NodeName, error) {
	out := make(map[string]NodeName, len(uuids))
	if len(uuids) == 0 {
		return out, nil
	}
	var nodes []Node
	if err := s.reader().Select("UUID", "Name", "Hostname").Where("uuid IN ?", uuids).Find(&nodes).Error; err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.Name != "" {
			out[n.UUID] = NodeName{Name: n.Name}
		} else {
			out[n.UUID] = NodeName{Name: n.Hostname}
		}
	}
	var missing []string
	for _, uuid := range uuids {
		if _, ok := out[uuid]; !ok {
			missing = append(missing, uuid)
		}
	}
	gone, err := s.DeletedNodes(missing)
	if err != nil {
		return nil, err
	}
	for uuid, n := range gone {
		out[uuid] = NodeName{Name: n.Name, Deleted: true}
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

// InventoryCard is a card the inventory holds now, with its node's identity.
type InventoryCard struct {
	NodeUUID string
	NodeName string
	Index    int
	UUID     string
	Model    string
}

// CardInventory is every card of every node, in lean columns: the GPU
// statistics page sizes its answer by the cards a filter covers.
func (s *Store) CardInventory() ([]InventoryCard, error) {
	db := s.reader()
	var nodes []Node
	if err := db.Select("ID", "UUID", "Name", "Hostname").Find(&nodes).Error; err != nil {
		return nil, err
	}
	byID := make(map[uint]*Node, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	var cards []GPU
	if err := db.Select("NodeID", "Index", "UUID", "Name").Find(&cards).Error; err != nil {
		return nil, err
	}
	out := make([]InventoryCard, 0, len(cards))
	for _, c := range cards {
		n := byID[c.NodeID]
		if n == nil {
			continue
		}
		name := n.Name
		if name == "" {
			name = n.Hostname
		}
		out = append(out, InventoryCard{NodeUUID: n.UUID, NodeName: name, Index: c.Index, UUID: c.UUID, Model: c.Name})
	}
	return out, nil
}
