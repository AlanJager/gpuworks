package store

import "gorm.io/gorm"

// FleetCapacity is how large the fleet is, as an enrollment guard judges a
// new node against it: the fleet before that node is added.
type FleetCapacity struct {
	// Nodes is every node row: online, offline and pending alike, from any
	// entry (SSH, a cloud provider, a Kubernetes cluster).
	Nodes int
	// NodesWithCards is the nodes with at least one card they may use.
	NodesWithCards int
	// OwnedCards is the cards the nodes may use (Node.GPUCount, which counts
	// only accelerator.CardUsable cards): a card handed to a VM or without
	// a driver is not the platform's and is not counted.
	OwnedCards int
}

// fleetCapacitySQL is one aggregate over the nodes table: a single scan,
// no rows loaded, no sort (fleet_capacity_test checks the query plan).
const fleetCapacitySQL = `SELECT COUNT(*) AS nodes,
	COALESCE(SUM(CASE WHEN gpu_count > 0 THEN 1 ELSE 0 END), 0) AS nodes_with_cards,
	COALESCE(SUM(gpu_count), 0) AS owned_cards
	FROM nodes`

// fleetCapacity counts the fleet on db: the writer transaction of a node
// insert, where the count and the insert cannot interleave with another
// insert, or the read pool for a pre-check.
func fleetCapacity(db *gorm.DB) (FleetCapacity, error) {
	var c FleetCapacity
	err := db.Raw(fleetCapacitySQL).Row().Scan(&c.Nodes, &c.NodesWithCards, &c.OwnedCards)
	return c, err
}
