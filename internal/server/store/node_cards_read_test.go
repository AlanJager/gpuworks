package store

import (
	"path/filepath"
	"sync"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A status report committed between the node query and its card preload must
// not pair the node row from before the report with the cards it wrote. The
// report is forced into exactly that gap by a query callback that runs after
// the node rows are read and before the preload.
func TestNodeListingsReadANodeAndItsCardsFromOneSnapshot(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	listings := map[string]func(n *Node) ([]Node, error){
		"ListNodes":          func(*Node) ([]Node, error) { return s.ListNodes() },
		"NodesByUUID":        func(n *Node) ([]Node, error) { return s.NodesByUUID([]string{n.UUID}) },
		"NodesForLabels":     func(*Node) ([]Node, error) { return s.NodesForLabels(true) },
		"NodesForLabelsByID": func(n *Node) ([]Node, error) { return s.NodesForLabelsByID([]uint{n.ID}, true) },
	}
	for name, list := range listings {
		t.Run(name, func(t *testing.T) {
			n, err := s.RegisterNode("uuid-"+name, HashToken("tok-"+name), "host-"+name)
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			var reportErr error
			const hook = "test:report-between-node-and-cards"
			if err := s.reader().Callback().Query().After("gorm:query").Before("gorm:preload").Register(hook, func(db *gorm.DB) {
				if db.Statement.Table != "nodes" {
					return
				}
				once.Do(func() {
					// The report the agent sends on connect: the runtime it
					// found and the card, in one write transaction.
					reportErr = s.db.Transaction(func(tx *gorm.DB) error {
						if err := tx.Model(&Node{}).Where("id = ?", n.ID).Update("runtime_driver", "docker").Error; err != nil {
							return err
						}
						return tx.Create(&GPU{NodeID: n.ID, Ident: "card-0", Name: "Fake"}).Error
					})
				})
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.reader().Callback().Query().Remove(hook) })

			nodes, err := list(n)
			if err != nil {
				t.Fatal(err)
			}
			if reportErr != nil {
				t.Fatalf("report in the gap: %v", reportErr)
			}
			var got *Node
			for i := range nodes {
				if nodes[i].ID == n.ID {
					got = &nodes[i]
				}
			}
			if got == nil {
				t.Fatalf("node %d missing from %s", n.ID, name)
			}
			// The listing began before the report, so it sees neither half.
			if len(got.GPUs) != 0 {
				t.Fatalf("%s paired the node row from before the report with %d card(s) the report wrote", name, len(got.GPUs))
			}
		})
	}
}
