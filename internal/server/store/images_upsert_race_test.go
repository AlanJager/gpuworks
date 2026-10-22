package store

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// raceStoreSeq keeps in-memory DSNs unique across test invocations so repeated
// runs (-count=N) never share a database with an earlier one.
var raceStoreSeq int64

// newRaceTestStore opens a store whose in-memory database is unique per test
// invocation, so repeated runs (-count=N) never observe rows from earlier ones.
func newRaceTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), atomic.AddInt64(&raceStoreSeq, 1))
	s, err := Open(dsn, bus.New())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A sync/pull start and a node status report can both create the same
// image_nodes row at the moment a node comes online. Neither writer may fail
// with a unique-constraint error, and exactly one row may exist afterwards.
func TestConcurrentImageNodeWritersDoNotConflict(t *testing.T) {
	const iterations = 25
	for _, tc := range []struct {
		name string
		race func(t *testing.T, s *Store, node *Node, image *Image, ref string) error
	}{
		{
			name: "sync start while the node reports presence",
			race: func(t *testing.T, s *Store, node *Node, image *Image, ref string) error {
				var syncErr, statusErr error
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, syncErr = s.UpsertImageNode(&ImageNode{ImageVersionID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "race-transfer"})
				}()
				go func() {
					defer wg.Done()
					<-start
					statusErr = s.UpdateNodeStatus(node.UUID, proto.NodeStatus{Images: []proto.ImagePresence{{ImageRef: ref, Present: true, SizeBytes: 128}}})
				}()
				close(start)
				wg.Wait()
				if syncErr != nil {
					return fmt.Errorf("sync upsert: %w", syncErr)
				}
				if statusErr != nil {
					return fmt.Errorf("status report: %w", statusErr)
				}
				return nil
			},
		},
		{
			name: "two sync starts at once",
			race: func(t *testing.T, s *Store, node *Node, image *Image, ref string) error {
				var firstErr, secondErr error
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, firstErr = s.UpsertImageNode(&ImageNode{ImageVersionID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "race-a"})
				}()
				go func() {
					defer wg.Done()
					<-start
					_, secondErr = s.UpsertImageNode(&ImageNode{ImageVersionID: image.ID, NodeID: node.ID, State: ImageStateSyncing, TransferID: "race-b"})
				}()
				close(start)
				wg.Wait()
				if firstErr != nil {
					return fmt.Errorf("first upsert: %w", firstErr)
				}
				if secondErr != nil {
					return fmt.Errorf("second upsert: %w", secondErr)
				}
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newRaceTestStore(t)
			node, err := s.RegisterNode("race-node", HashToken("tok"), "race-host")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < iterations; i++ {
				ref := fmt.Sprintf("example.com/race-%s:%d", t.Name(), i)
				// t.Name() contains a slash from the subtest; keep the tag valid.
				ref = sanitizeRaceRef(ref)
				image, err := s.UpsertImage(&Image{ImageRef: ref, Engine: "vllm", Name: "race", Enabled: true})
				if err != nil {
					t.Fatalf("image %d: %v", i, err)
				}
				if err := tc.race(t, s, node, image, ref); err != nil {
					t.Fatalf("iteration %d: %v", i, err)
				}
				rows, err := s.ImageNodesForImage(image.ID)
				if err != nil {
					t.Fatalf("list iteration %d: %v", i, err)
				}
				if len(rows) != 1 {
					t.Fatalf("iteration %d: %d rows for one (image, node), want 1: %+v", i, len(rows), rows)
				}
			}
		})
	}
}

func sanitizeRaceRef(ref string) string {
	out := make([]rune, 0, len(ref))
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ':', r == '/':
			out = append(out, r)
		default:
			out = append(out, '-')
		}
	}
	return string(out)
}
