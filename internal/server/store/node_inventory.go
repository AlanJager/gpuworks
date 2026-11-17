package store

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
)

// NodeInventoryCard is one card a node may use (accelerator.CardUsable), as
// an inventory shows it. It holds no serial: those leave the server only
// salted (NodeIdentity).
type NodeInventoryCard struct {
	// Index is the vendor CLI's device index (GPU.Index), -1 when the CLI
	// does not list the card.
	Index int
	// Model is the card's model name as the probe reported it (GPU.Name).
	Model string
	// Serial reports whether the card carries a hardware serial
	// (accelerator.CardSerial) and so takes part in NodeIdentity.Cards.
	Serial bool
}

// NodeInventoryNode is one node of the inventory.
type NodeInventoryNode struct {
	ID   uint
	UUID string
	// Name is what a person calls the node (Node.DisplayName).
	Name string
	// CreatedAt is when the node enrolled; with ID it gives the
	// enrollment order.
	CreatedAt time.Time
	// HostReported reports whether the node's agent reported the machine's
	// system UUID, which is what NodeIdentity.Host digests.
	HostReported bool
	// Cards are the cards the node may use, by device index.
	Cards []NodeInventoryCard
}

// NodeInventoryPage is one page of NodeInventory.
type NodeInventoryPage struct {
	Nodes []NodeInventoryNode
	// Next is the cursor of the following page, 0 when this one is the last.
	Next uint
}

// NodeInventory is a page of every node in enrollment order (ascending id),
// starting after the node whose id is cursor (0 for the first page), at
// most limit nodes (1..MaxNodeReadBatch, else ErrNodeReadBatch). Ids are
// never reused, so a node enrolled or deleted between pages neither shifts
// nor repeats the rest. Each page is two indexed reads on the read pool:
// nodes by primary-key range and the cards of those nodes.
func (s *Store) NodeInventory(ctx context.Context, cursor uint, limit int) (NodeInventoryPage, error) {
	if limit < 1 || limit > MaxNodeReadBatch {
		return NodeInventoryPage{}, ErrNodeReadBatch
	}
	var rows []nodeInventoryRow
	if err := s.reader().WithContext(ctx).Raw(nodeInventorySQL, cursor, limit+1).Scan(&rows).Error; err != nil {
		return NodeInventoryPage{}, err
	}
	page := NodeInventoryPage{Nodes: []NodeInventoryNode{}}
	if len(rows) > limit {
		rows = rows[:limit]
		page.Next = rows[limit-1].ID
	}
	if len(rows) == 0 {
		return page, nil
	}
	ids := make([]uint, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	cards, err := s.ownedCards(ctx, ids)
	if err != nil {
		return NodeInventoryPage{}, err
	}
	page.Nodes = make([]NodeInventoryNode, len(rows))
	for i, r := range rows {
		n := NodeInventoryNode{
			ID: r.ID, UUID: r.UUID, CreatedAt: r.CreatedAt, HostReported: r.HostSystemUUID != "",
			Name:  Node{UUID: r.UUID, Name: r.Name, Hostname: r.Hostname}.DisplayName(),
			Cards: make([]NodeInventoryCard, 0, len(cards[r.ID])),
		}
		for _, c := range cards[r.ID] {
			_, serial := accelerator.CardSerial(c.UUID)
			n.Cards = append(n.Cards, NodeInventoryCard{Index: c.CardIndex, Model: c.Name, Serial: serial})
		}
		page.Nodes[i] = n
	}
	return page, nil
}

// nodeInventorySQL reads one page of nodes by primary-key range; it asks
// for one row more than the page to know whether another follows.
const nodeInventorySQL = `SELECT id, uuid, name, hostname, created_at, host_system_uuid FROM nodes WHERE id > ? ORDER BY id LIMIT ?`

// nodeInventoryRow is one node row NodeInventory reads.
type nodeInventoryRow struct {
	ID             uint
	UUID           string
	Name           string
	Hostname       string
	CreatedAt      time.Time
	HostSystemUUID string
}
