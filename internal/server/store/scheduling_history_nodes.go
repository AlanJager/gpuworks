package store

import "sort"

// EventParamNodeUUIDs is the param a scheduling history event names the
// nodes it concerns by: a list of node UUIDs. The instances an event names
// are removed in time, so where they ran cannot be read back later; the
// event keeps it. A few events already named their node before this param
// existed: DEPLOYMENT_UNSCHEDULABLE's "node" is the node's UUID, and older
// REPLICA_REBUILT and NODE_REPLACE_NO_CANDIDATE rows name nodes by display
// name ("node", "nodes").
const EventParamNodeUUIDs = "node_uuids"

// NodeRef names a node: its UUID and display name (Node.DisplayName).
type NodeRef struct {
	UUID string
	Name string
}

// NodeRefsByID is the UUID and display name of the nodes of ids, one query of
// the columns they are made from; an unknown id is absent.
func (s *Store) NodeRefsByID(ids []uint) (map[uint]NodeRef, error) {
	out := map[uint]NodeRef{}
	if len(ids) == 0 {
		return out, nil
	}
	var nodes []Node
	if err := s.reader().Select("id", "uuid", "name", "hostname").Where("id IN ?", ids).Find(&nodes).Error; err != nil {
		return nil, err
	}
	for _, n := range nodes {
		out[n.ID] = NodeRef{UUID: n.UUID, Name: n.DisplayName()}
	}
	return out, nil
}

// NodeUUIDsOfInstances is the UUIDs of the nodes the named instances are
// placed on, sorted and each once: one indexed read of the instances and
// their nodes. An instance not placed or no longer known adds nothing.
func (s *Store) NodeUUIDsOfInstances(uuids []string) ([]string, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	var out []string
	err := s.reader().Table("instances").Distinct("nodes.uuid").
		Joins("JOIN nodes ON nodes.id = instances.node_id").
		Where("instances.uuid IN ?", uuids).Pluck("nodes.uuid", &out).Error
	sort.Strings(out)
	return out, err
}

// eventNodeRefsQuery is EventNodeRefs' one read: the nodes by UUID, then
// the instances' nodes.
const eventNodeRefsQuery = `SELECT '' AS instance, uuid, name, hostname FROM nodes WHERE uuid IN (?)
UNION ALL
SELECT instances.uuid AS instance, nodes.uuid, nodes.name, nodes.hostname
FROM instances JOIN nodes ON nodes.id = instances.node_id WHERE instances.uuid IN (?)`

// EventNodeRefs resolves the nodes a page of scheduling history names, in one
// query whatever the page holds: the nodes of nodeUUIDs by UUID, and the
// nodes the live instances of instanceUUIDs are placed on. Both reads go by
// a unique index, so the cost is the page's, not the fleet's. A UUID with no
// node, or an instance no longer known or not placed, is absent.
func (s *Store) EventNodeRefs(nodeUUIDs, instanceUUIDs []string) (byUUID, byInstance map[string]NodeRef, err error) {
	byUUID, byInstance = map[string]NodeRef{}, map[string]NodeRef{}
	if len(nodeUUIDs) == 0 && len(instanceUUIDs) == 0 {
		return byUUID, byInstance, nil
	}
	// An empty list is no list: IN over a NULL matches nothing.
	none := func(list []string) any {
		if len(list) == 0 {
			return nil
		}
		return list
	}
	var rows []struct {
		Instance string
		UUID     string
		Name     string
		Hostname string
	}
	err = s.reader().Raw(eventNodeRefsQuery, none(nodeUUIDs), none(instanceUUIDs)).Scan(&rows).Error
	if err != nil {
		return nil, nil, err
	}
	for _, row := range rows {
		ref := NodeRef{UUID: row.UUID, Name: Node{UUID: row.UUID, Name: row.Name, Hostname: row.Hostname}.DisplayName()}
		if row.Instance == "" {
			byUUID[row.UUID] = ref
		} else {
			byInstance[row.Instance] = ref
		}
	}
	return byUUID, byInstance, nil
}

// EventInstances is the instances an event's params name ("instance",
// "instance_uuid", "instances"), after the instance the row is on, if any;
// each once, in order.
func EventInstances(rowInstance string, params map[string]any) []string {
	var out []string
	seen := map[string]bool{}
	add := func(uuid string) {
		if uuid != "" && !seen[uuid] {
			seen[uuid] = true
			out = append(out, uuid)
		}
	}
	add(rowInstance)
	for _, key := range []string{"instance", "instance_uuid"} {
		if uuid, ok := params[key].(string); ok {
			add(uuid)
		}
	}
	for _, uuid := range paramStrings(params["instances"]) {
		add(uuid)
	}
	return out
}

// paramStrings reads a list of strings from a param, as written ([]string)
// or as decoded from JSON ([]any).
func paramStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

// EventNodeUUIDs is the node UUIDs an event's params name (EventParamNodeUUIDs).
func EventNodeUUIDs(params map[string]any) []string {
	return paramStrings(params[EventParamNodeUUIDs])
}
