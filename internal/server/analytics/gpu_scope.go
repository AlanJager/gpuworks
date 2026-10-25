package analytics

import (
	"sort"
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type cardKey struct {
	node  string
	index int
	uuid  string
}

// gpuScope is what a GPU query covers in the inventory now: the cards its
// filter matches, every card the inventory holds, and the deployments on
// them.
type gpuScope struct {
	cards []store.InventoryCard
	// fleet is every card in the inventory.
	fleet int
	// names are every node's names, by UUID; deleted are the nodes among
	// them that no longer exist.
	names   map[string]string
	deleted map[string]bool
	// present are the inventory's cards, whether their node is online or
	// not: a card leaves it when its node no longer reports it, or the node
	// is deleted.
	present map[cardKey]bool
	holders map[cardKey][]string
}

func (s *Service) gpuScope(q GPUQuery) (*gpuScope, error) {
	inv, err := s.Store.CardInventory()
	if err != nil {
		return nil, err
	}
	var only map[string]bool
	if q.NodeUUIDs != nil {
		only = make(map[string]bool, len(q.NodeUUIDs))
		for _, n := range q.NodeUUIDs {
			only[n] = true
		}
	}
	sc := &gpuScope{fleet: len(inv), names: map[string]string{}, deleted: map[string]bool{}, present: make(map[cardKey]bool, len(inv))}
	for _, c := range inv {
		sc.names[c.NodeUUID] = c.NodeName
		sc.present[cardKey{c.NodeUUID, c.Index, c.UUID}] = true
		if (only == nil || only[c.NodeUUID]) && (q.GPUModel == "" || c.Model == q.GPUModel) {
			sc.cards = append(sc.cards, c)
		}
	}
	sc.holders, err = s.holdersNow()
	return sc, err
}

// nodes are the UUIDs of the nodes of the scope's cards.
func (sc *gpuScope) nodes() []string {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range sc.cards {
		if !seen[c.NodeUUID] {
			seen[c.NodeUUID] = true
			out = append(out, c.NodeUUID)
		}
	}
	return out
}

// byModel counts the scope's cards of each model.
func (sc *gpuScope) byModel() []dto.GPUModelCount {
	counts := map[string]int{}
	for _, c := range sc.cards {
		counts[c.Model]++
	}
	out := make([]dto.GPUModelCount, 0, len(counts))
	for m, n := range counts {
		out = append(out, dto.GPUModelCount{Model: m, Cards: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// nameNodes names the nodes of uuids the inventory no longer holds too:
// a deleted node by the name it left, marked deleted.
func (s *Service) nameNodes(sc *gpuScope, uuids []string) error {
	var missing []string
	for _, uuid := range uuids {
		if _, ok := sc.names[uuid]; !ok && !contains(missing, uuid) {
			missing = append(missing, uuid)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	more, err := s.Store.NodeNamesByUUID(missing)
	if err != nil {
		return err
	}
	for uuid, n := range more {
		sc.names[uuid] = n.Name
		sc.deleted[uuid] = n.Deleted
	}
	return nil
}

// groupRows are the table's rows of the groups' statistics, with the
// scope's cards, nodes, models and holders of each.
func (sc *gpuScope) groupRows(by statsdb.GPUGroupBy, groups map[string]rollup.GPU) []dto.GPUGroupRow {
	type acc struct {
		cards   int
		free    int
		nodes   map[string]bool
		models  map[string]bool
		holders map[string]int
	}
	accs := map[string]*acc{}
	for _, c := range sc.cards {
		key := c.Model
		if by == statsdb.GPUByNode {
			key = c.NodeUUID
		}
		a := accs[key]
		if a == nil {
			a = &acc{nodes: map[string]bool{}, models: map[string]bool{}, holders: map[string]int{}}
			accs[key] = a
		}
		a.cards++
		a.nodes[c.NodeUUID] = true
		a.models[c.Model] = true
		held := sc.holders[cardKey{c.NodeUUID, c.Index, c.UUID}]
		if len(held) == 0 {
			a.free++
		}
		for _, d := range held {
			a.holders[d]++
		}
	}
	out := make([]dto.GPUGroupRow, 0, len(groups))
	for key, g := range groups {
		row := dto.GPUGroupRow{Key: key, Label: sc.heatLabel(by, key), Models: []string{}, Deployments: []dto.GPUHolding{}, GPUFigures: gpuFigures(g)}
		if by == statsdb.GPUByNode {
			row.NodeDeleted = sc.deleted[key]
		}
		if a := accs[key]; a != nil {
			row.Cards, row.Nodes, row.Unassigned = a.cards, len(a.nodes), a.free
			for m := range a.models {
				row.Models = append(row.Models, m)
			}
			sort.Strings(row.Models)
			for name, n := range a.holders {
				row.Deployments = append(row.Deployments, dto.GPUHolding{Name: name, Cards: n})
			}
			sort.Slice(row.Deployments, func(i, j int) bool { return row.Deployments[i].Name < row.Deployments[j].Name })
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func (sc *gpuScope) heatLabel(by statsdb.GPUGroupBy, group string) string {
	switch by {
	case statsdb.GPUByNode:
		if n := sc.names[group]; n != "" {
			return n
		}
	case statsdb.GPUByCard:
		return cardLabel(group, sc.names)
	case statsdb.GPUByModel:
		if group == "" {
			return "?"
		}
	}
	return group
}

// holdersNow are the deployments that hold each card of the online nodes
// now.
func (s *Service) holdersNow() (map[cardKey][]string, error) {
	snap, err := s.Store.GPUTimeSnapshot()
	if err != nil {
		return nil, err
	}
	byIdent := map[string]cardKey{}
	for _, c := range snap.Cards {
		node := snap.NodeUUIDs[c.NodeID]
		k := cardKey{node, c.Index, c.UUID}
		byIdent[strconv.FormatUint(uint64(c.NodeID), 10)+"\x00"+c.Ident] = k
	}
	deps := map[uint]bool{}
	for _, h := range snap.Holdings {
		deps[h.DeploymentID] = true
	}
	names, err := s.deploymentNames(deps)
	if err != nil {
		return nil, err
	}
	holders := map[cardKey][]string{}
	for _, h := range snap.Holdings {
		name := names[h.DeploymentID].Name
		for _, ident := range h.Idents {
			if k, ok := byIdent[strconv.FormatUint(uint64(h.NodeID), 10)+"\x00"+ident]; ok && !contains(holders[k], name) {
				holders[k] = append(holders[k], name)
			}
		}
	}
	return holders, nil
}
