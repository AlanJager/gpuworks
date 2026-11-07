package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

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

// The writer-pool lookups hold its single connection across both statements,
// so a report sent in the gap cannot commit until the lookup has read both:
// it is still waiting when the lookup returns, and lands right after.
func TestWriterPoolNodeLookupsKeepAReportOutOfTheGap(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	lookups := map[string]func(n *Node) (*Node, error){
		"NodeByID":     func(n *Node) (*Node, error) { return s.NodeByID(n.ID) },
		"NodeWithGPUs": func(n *Node) (*Node, error) { return s.NodeWithGPUs(n.UUID) },
	}
	for name, lookup := range lookups {
		t.Run(name, func(t *testing.T) {
			n, err := s.RegisterNode("uuid-"+name, HashToken("tok-"+name), "host-"+name)
			if err != nil {
				t.Fatal(err)
			}
			var once sync.Once
			reported := make(chan error, 1)
			const hook = "test:report-between-node-and-cards-writer"
			if err := s.db.Callback().Query().After("gorm:query").Before("gorm:preload").Register(hook, func(db *gorm.DB) {
				if db.Statement.Table != "nodes" {
					return
				}
				once.Do(func() {
					go func() {
						reported <- s.db.Transaction(func(tx *gorm.DB) error {
							if err := tx.Model(&Node{}).Where("id = ?", n.ID).Update("runtime_driver", "docker").Error; err != nil {
								return err
							}
							return tx.Create(&GPU{NodeID: n.ID, Ident: "card-0", Name: "Fake"}).Error
						})
					}()
					// Give the report every chance to slip in before the
					// preload; holding the connection must keep it out.
					select {
					case err := <-reported:
						reported <- err
					case <-time.After(200 * time.Millisecond):
					}
				})
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.db.Callback().Query().Remove(hook) })

			got, err := lookup(n)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.GPUs) != 0 || got.RuntimeDriver != "" {
				t.Fatalf("%s read runtime %q with %d card(s): a report committed between the node and its cards", name, got.RuntimeDriver, len(got.GPUs))
			}
			select {
			case err := <-reported:
				if err != nil {
					t.Fatalf("report after the lookup: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the report never committed after the lookup released the connection")
			}
		})
	}
}
