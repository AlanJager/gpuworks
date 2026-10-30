package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/proto"
)

// simulatedNode is a node runtime standing for another machine
// (hostsim.Host), whose models directory holds a file, with a file outside
// it to reach for, and the messages it emits.
type simulatedNode struct {
	*NodeRuntime
	driver          *candidateDriver
	inside, outside string

	mu   sync.Mutex
	sent []any
}

func newSimulatedNode(t *testing.T, simulated bool) *simulatedNode {
	t.Helper()
	root := t.TempDir()
	driver := newCandidateDriver()
	n := NewNodeRuntime(slog.Default(), namedPullDriver{driver, "docker"}, filepath.Join(root, "node", "models"))
	s := &simulatedNode{NodeRuntime: n, driver: driver,
		inside: filepath.Join(n.ModelsRoot(), "tokenizer.json"), outside: filepath.Join(root, "secret.txt")}
	if err := os.MkdirAll(n.ModelsRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{s.inside: `{"model":{}}`, s.outside: "host secret"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if simulated {
		n.host = &hostsim.Host{Dir: n.ModelsRoot()}
	}
	n.Bind(func(_ string, body any) error {
		s.mu.Lock()
		s.sent = append(s.sent, body)
		s.mu.Unlock()
		return nil
	})
	return s
}

func (s *simulatedNode) modelFileStatuses() []proto.ModelFileStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []proto.ModelFileStatus
	for _, b := range s.sent {
		if st, ok := b.(proto.ModelFileStatus); ok {
			out = append(out, st)
		}
	}
	return out
}

func TestSimulatedNodeStreamsOnlyItsOwnFiles(t *testing.T) {
	s := newSimulatedNode(t, true)
	var buf bytes.Buffer
	if err := s.StreamLargeFile(context.Background(), s.inside, &buf); err != nil || !strings.Contains(buf.String(), "model") {
		t.Fatalf("inside: err=%v body=%q", err, buf.String())
	}
	buf.Reset()
	if err := s.StreamLargeFile(context.Background(), s.outside, &buf); !errors.Is(err, hostsim.ErrRefused) || buf.Len() != 0 {
		t.Fatalf("outside: err=%v wrote %d bytes", err, buf.Len())
	}
	link := filepath.Join(s.ModelsRoot(), "link.json")
	if err := os.Symlink(s.outside, link); err != nil {
		t.Fatal(err)
	}
	if err := s.StreamLargeFile(context.Background(), link, &buf); !errors.Is(err, hostsim.ErrRefused) || buf.Len() != 0 {
		t.Fatalf("link out: err=%v wrote %d bytes", err, buf.Len())
	}

	real := newSimulatedNode(t, false)
	if err := real.StreamLargeFile(context.Background(), real.outside, &buf); err != nil || buf.String() != "host secret" {
		t.Fatalf("real node: err=%v body=%q", err, buf.String())
	}
}

// TestSimulatedNodeConfinesWhatTheServerPushes: a desired state cannot move
// a simulated node's models root or make it download; the download is
// failed, not left waiting.
func TestSimulatedNodeConfinesWhatTheServerPushes(t *testing.T) {
	s := newSimulatedNode(t, true)
	root := s.ModelsRoot()
	task := proto.ModelFileTask{Key: "m1", DesiredState: "present", Source: "url", Endpoint: "https://example.com/m", RelPath: "m"}
	s.OnStateSync(context.Background(), proto.DesiredState{ModelsRoot: "/", ModelFiles: []proto.ModelFileTask{task}})
	if s.ModelsRoot() != root {
		t.Fatalf("models root moved to %q", s.ModelsRoot())
	}
	s.OnStateUpdate(context.Background(), proto.StateUpdate{ModelsRoot: "/", ModelFiles: []proto.ModelFileTask{{Key: "m2", DesiredState: "present", RelPath: "m2"}}})
	if s.ModelsRoot() != root {
		t.Fatalf("models root moved to %q", s.ModelsRoot())
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(s.modelFileStatuses()) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	got := s.modelFileStatuses()
	if len(got) != 2 {
		t.Fatalf("statuses = %+v", got)
	}
	for _, st := range got {
		if st.State != proto.MFFailed || !strings.Contains(st.Message, hostsim.ErrRefused.Error()) {
			t.Fatalf("status = %+v, want refused", st)
		}
	}
	if s.isDownloading("m1") || s.isDownloading("m2") {
		t.Fatal("a refused download started")
	}

	real := newSimulatedNode(t, false)
	real.OnStateSync(context.Background(), proto.DesiredState{ModelsRoot: "/srv/models"})
	if real.ModelsRoot() != "/srv/models" {
		t.Fatalf("real node models root = %q", real.ModelsRoot())
	}
}

func TestSimulatedNodeAnswersImagePullsWithoutARegistry(t *testing.T) {
	s := newSimulatedNode(t, true)
	params, _ := json.Marshal(map[string]any{"effective_ref": "registry.example.com/x:1", "canonical_ref": "x:1", "transfer_id": "t"})
	if _, err := s.OnRPC(context.Background(), proto.RPCImagePull, params); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		var present bool
		for _, b := range s.sent {
			if st, ok := b.(proto.ImageNodeStatus); ok && st.State == "present" {
				present = true
			}
		}
		s.mu.Unlock()
		if present {
			s.driver.mu.Lock()
			pulled := s.driver.pulled
			s.driver.mu.Unlock()
			if len(pulled) != 0 {
				t.Fatalf("the simulated node pulled %v", pulled)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the image was not reported present")
}
