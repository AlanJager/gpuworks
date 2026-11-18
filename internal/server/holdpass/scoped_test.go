package holdpass

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// scopes records the scope of each pass and fails the first failN.
type scopes struct {
	mu    sync.Mutex
	got   [][]uint
	failN int
	ran   chan struct{}
}

func (s *scopes) apply(nodes []uint) ([]string, []string, error) {
	s.mu.Lock()
	s.got = append(s.got, nodes)
	fail := s.failN > 0
	if fail {
		s.failN--
	}
	s.mu.Unlock()
	s.ran <- struct{}{}
	if fail {
		return nil, nil, errors.New("database is locked")
	}
	return nil, nil, nil
}

func (s *scopes) wait(t *testing.T) []uint {
	t.Helper()
	select {
	case <-s.ran:
	case <-time.After(2 * time.Second):
		t.Fatal("no pass")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[len(s.got)-1]
}

// Scoped requests made between two passes are merged into one pass over
// their nodes, ascending; a request for every node in the meantime makes it
// a fleet pass; and an empty request asks for nothing.
func TestScopedRequestsMergeIntoOnePass(t *testing.T) {
	s := &scopes{ran: make(chan struct{}, 16)}
	r := NewScoped(s.apply, quiet, 50*time.Millisecond)
	r.RequestNodes(nil)
	r.RequestNodes([]uint{9, 3})
	r.RequestNodes([]uint{3, 5})
	stop := start(t, r)
	defer stop()
	if got := s.wait(t); !slices.Equal(got, []uint{3, 5, 9}) {
		t.Fatalf("pass over %v, want [3 5 9]", got)
	}
	r.RequestNodes([]uint{7})
	r.Request()
	r.RequestNodes([]uint{8})
	if got := s.wait(t); got != nil {
		t.Fatalf("pass over %v, want the fleet", got)
	}
	select {
	case <-s.ran:
		t.Fatal("a pass ran with nothing requested")
	case <-time.After(200 * time.Millisecond):
	}
}

// Naming more than MaxScoped nodes between two passes makes a fleet pass.
func TestTooManyScopedNodesMakeAFleetPass(t *testing.T) {
	s := &scopes{ran: make(chan struct{}, 4)}
	r := NewScoped(s.apply, quiet, time.Millisecond)
	ids := make([]uint, MaxScoped+1)
	for i := range ids {
		ids[i] = uint(i + 1)
	}
	r.RequestNodes(ids[:MaxScoped])
	r.RequestNodes(ids[MaxScoped:])
	stop := start(t, r)
	defer stop()
	if got := s.wait(t); got != nil {
		t.Fatalf("pass over %d nodes, want the fleet", len(got))
	}
}

// A failed scoped pass is retried over the same nodes, joined by those
// requested meanwhile.
func TestAFailedScopedPassIsRetriedOverItsNodes(t *testing.T) {
	s := &scopes{ran: make(chan struct{}, 4), failN: 1}
	r := NewScoped(s.apply, quiet, 50*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	r.RequestNodes([]uint{4})
	if got := s.wait(t); !slices.Equal(got, []uint{4}) {
		t.Fatalf("pass over %v, want [4]", got)
	}
	r.RequestNodes([]uint{2})
	if got := s.wait(t); !slices.Equal(got, []uint{2, 4}) {
		t.Fatalf("retry over %v, want [2 4]", got)
	}
}
