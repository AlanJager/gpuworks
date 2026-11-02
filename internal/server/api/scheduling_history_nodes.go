package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// historyNodeKeys is what one entry's params say about its nodes.
type historyNodeKeys struct {
	// uuids are node UUIDs (store.EventParamNodeUUIDs).
	uuids []string
	// node is the single "node" param: a node's UUID, or on older rows its
	// display name.
	node string
	// names are display names only (an older row's "nodes").
	names []string
}

func (k historyNodeKeys) any() bool { return len(k.uuids) > 0 || k.node != "" || len(k.names) > 0 }

// historyNodeKeysOf reads an entry's node params; the UUID list wins over the
// older keys.
func historyNodeKeysOf(params map[string]any) historyNodeKeys {
	var keys historyNodeKeys
	if keys.uuids = store.EventNodeUUIDs(params); len(keys.uuids) > 0 {
		return keys
	}
	if node, ok := params["node"].(string); ok && node != "" {
		keys.node = node
		return keys
	}
	if list, ok := params["nodes"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok && name != "" {
				keys.names = append(keys.names, name)
			}
		}
	}
	return keys
}

// historyNodes names the node or nodes each entry of a history page
// concerns, in one read for the whole page (store.EventNodeRefs). For each
// entry, the first of these that names any:
//
//   - the nodes its params name: store.EventParamNodeUUIDs, "node" (a UUID,
//     or the display name older rows kept) and, on older rows, "nodes"
//     (display names);
//   - the nodes the instances it names are on now, while they still exist;
//   - none.
//
// A node the server knows is named by its UUID and display name. One it no
// longer knows, or that an older row named only by name, has a name and no
// UUID: there is nothing to open.
func (a *apiHandlers) historyNodes(events []store.InstanceEvent, params []map[string]any) ([][]dto.SchedulingHistoryNode, error) {
	keys := make([]historyNodeKeys, len(events))
	var nodeUUIDs, instanceUUIDs []string
	for i, ev := range events {
		keys[i] = historyNodeKeysOf(params[i])
		nodeUUIDs = append(nodeUUIDs, keys[i].uuids...)
		if keys[i].node != "" {
			nodeUUIDs = append(nodeUUIDs, keys[i].node)
		}
		if !keys[i].any() {
			instanceUUIDs = append(instanceUUIDs, store.EventInstances(ev.InstanceUUID, params[i])...)
		}
	}
	byUUID, byInstance, err := a.d.Store.EventNodeRefs(nodeUUIDs, instanceUUIDs)
	if err != nil {
		return nil, err
	}
	out := make([][]dto.SchedulingHistoryNode, len(events))
	for i, ev := range events {
		var refs []dto.SchedulingHistoryNode
		add := func(ref dto.SchedulingHistoryNode) {
			for _, have := range refs {
				if have == ref {
					return
				}
			}
			refs = append(refs, ref)
		}
		known := func(uuid string) bool {
			ref, ok := byUUID[uuid]
			if ok {
				add(dto.SchedulingHistoryNode{UUID: ref.UUID, Name: ref.Name})
			}
			return ok
		}
		k := keys[i]
		for _, uuid := range k.uuids {
			if !known(uuid) {
				// A node that is gone: its short UUID, as a display name
				// falls back to.
				add(dto.SchedulingHistoryNode{Name: store.Node{UUID: uuid}.DisplayName()})
			}
		}
		if k.node != "" && !known(k.node) {
			add(dto.SchedulingHistoryNode{Name: k.node})
		}
		for _, name := range k.names {
			add(dto.SchedulingHistoryNode{Name: name})
		}
		if !k.any() {
			for _, instance := range store.EventInstances(ev.InstanceUUID, params[i]) {
				if ref, ok := byInstance[instance]; ok {
					add(dto.SchedulingHistoryNode{UUID: ref.UUID, Name: ref.Name})
				}
			}
		}
		out[i] = refs
	}
	return out, nil
}
