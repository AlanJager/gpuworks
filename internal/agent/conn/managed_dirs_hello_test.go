package conn

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// helloInventoryHandlers is a handler that reports what the node holds.
type helloInventoryHandlers struct {
	streamTestHandlers
	assets, groups []string
	managedDirs    bool
}

func (h *helloInventoryHandlers) HelloInventory() ([]string, []string, bool) {
	return h.assets, h.groups, h.managedDirs
}

func helloFrom(t *testing.T, handlers Handlers) proto.Hello {
	t.Helper()
	s := &laneTestServer{sessionID: "sess-1"}
	srv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(srv.Close)
	c := New(Credentials{ServerURL: srv.URL, NodeUUID: "node-1", NodeToken: "secret"}, nil, handlers, capabilityProber{}, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		if len(s.agentHellos) > 0 {
			hello := s.agentHellos[0]
			s.mu.Unlock()
			return hello
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("no hello reached the server")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A node that can hold managed directories says so and lists the groups it
// holds, beside its asset cache; one without an engine claims neither.
func TestHelloReportsManagedDirGroupsAndTheCapability(t *testing.T) {
	hello := helloFrom(t, &helloInventoryHandlers{assets: []string{"abc"}, groups: []string{"7-1700000000"}, managedDirs: true})
	if !slices.Contains(hello.Caps, proto.CapManagedDirs) {
		t.Fatalf("caps = %v, want %s", hello.Caps, proto.CapManagedDirs)
	}
	if !slices.Equal(hello.ManagedDirGroups, []string{"7-1700000000"}) || !slices.Equal(hello.RuntimeAssetDigests, []string{"abc"}) {
		t.Fatalf("hello = groups %v assets %v", hello.ManagedDirGroups, hello.RuntimeAssetDigests)
	}

	hello = helloFrom(t, &helloInventoryHandlers{})
	if slices.Contains(hello.Caps, proto.CapManagedDirs) || hello.ManagedDirGroups != nil {
		t.Fatalf("a node without an engine claimed managed directories: caps %v groups %v", hello.Caps, hello.ManagedDirGroups)
	}
}
