package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// simulatedProber stands for another machine (gpu.HostSimulator).
type simulatedProber struct{ *gpu.FixedProber }

func (simulatedProber) SimulatesHost() bool { return true }

// simulatedNode is a node runtime in simulation mode whose directory holds
// a model file, and a file outside it to reach for.
func simulatedNode(t *testing.T) (n *NodeRuntime, inside, outside string) {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "node")
	if err := os.MkdirAll(filepath.Join(state, "models"), 0o700); err != nil {
		t.Fatal(err)
	}
	inside = filepath.Join(state, "models", "tokenizer.json")
	outside = filepath.Join(root, "secret.txt")
	for path, data := range map[string]string{inside: `{"model":{}}`, outside: "host secret"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &NodeRuntime{prober: simulatedProber{&gpu.FixedProber{VendorName: "nvidia"}}, stateDir: state}, inside, outside
}

func pathParams(path string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"path": path})
	return raw
}

func TestSimulatedNodeAnswersOnlyAllowlistedRPCs(t *testing.T) {
	n, inside, _ := simulatedNode(t)
	for method := range simulatedNodeRPCs {
		params := json.RawMessage(`{}`)
		if pathRPCs[method] {
			params = pathParams(inside)
		}
		if err := n.simulatedHostRefusal(method, params); err != nil {
			t.Errorf("%s refused: %v", method, err)
		}
	}
	for _, method := range []string{
		proto.RPCNodeDoctor, proto.RPCAgentUpgrade, proto.RPCStorageProbe, proto.RPCStorageScan,
		proto.RPCStorageFilesList, proto.RPCStorageFilesDelete, "no.such.rpc",
	} {
		err := n.simulatedHostRefusal(method, json.RawMessage(`{}`))
		if !errors.Is(err, errSimulatedHost) {
			t.Errorf("%s: err = %v, want refused", method, err)
		}
	}
	// The refusal comes before any handler runs.
	if _, err := n.OnRPC(context.Background(), proto.RPCNodeDoctor, json.RawMessage(`{}`)); !errors.Is(err, errSimulatedHost) {
		t.Fatalf("OnRPC doctor: err = %v", err)
	}
}

func TestSimulatedNodeConfinesPathRPCsToItsDirectory(t *testing.T) {
	n, inside, outside := simulatedNode(t)
	escape := filepath.Join(n.stateDir, "models", "escape")
	if err := os.Symlink(filepath.Dir(outside), escape); err != nil {
		t.Fatal(err)
	}
	for method := range pathRPCs {
		for path, ok := range map[string]bool{
			inside: true,
			filepath.Join(n.stateDir, "models", "not-yet", "x"): true, // judged by where it would be
			n.stateDir:                          true,
			outside:                             false,
			"/etc/passwd":                       false,
			filepath.Join(n.stateDir, ".."):     false,
			filepath.Join(escape, "secret.txt"): false, // a link that leads out
			"models/tokenizer.json":             false, // relative
			"":                                  false,
		} {
			err := n.simulatedHostRefusal(method, pathParams(path))
			if ok && err != nil {
				t.Errorf("%s %q refused: %v", method, path, err)
			}
			if !ok && !errors.Is(err, errSimulatedHost) {
				t.Errorf("%s %q: err = %v, want refused", method, path, err)
			}
		}
		if err := n.simulatedHostRefusal(method, json.RawMessage(`{"path":`)); err == nil {
			t.Errorf("%s: malformed params accepted", method)
		}
	}
	if _, err := n.OnRPC(context.Background(), proto.RPCFSReadFile, pathParams(outside)); !errors.Is(err, errSimulatedHost) {
		t.Fatalf("OnRPC read outside: err = %v", err)
	}
}

func TestRealNodeIsNotConfined(t *testing.T) {
	_, _, outside := simulatedNode(t)
	n := &NodeRuntime{prober: &gpu.FixedProber{VendorName: "nvidia"}, stateDir: t.TempDir()}
	for _, method := range []string{proto.RPCNodeDoctor, proto.RPCAgentUpgrade, proto.RPCFSReadFile} {
		if err := n.simulatedHostRefusal(method, pathParams(outside)); err != nil {
			t.Fatalf("real node %s refused: %v", method, err)
		}
	}
	var buf bytes.Buffer
	if err := n.StreamLargeFile(context.Background(), outside, &buf); err != nil || buf.String() != "host secret" {
		t.Fatalf("real node large file: err=%v body=%q", err, buf.String())
	}
}

func TestSimulatedNodeStreamsOnlyItsOwnFiles(t *testing.T) {
	n, inside, outside := simulatedNode(t)
	var buf bytes.Buffer
	if err := n.StreamLargeFile(context.Background(), inside, &buf); err != nil || !strings.Contains(buf.String(), "model") {
		t.Fatalf("inside: err=%v body=%q", err, buf.String())
	}
	buf.Reset()
	err := n.StreamLargeFile(context.Background(), outside, &buf)
	if !errors.Is(err, errSimulatedHost) || buf.Len() != 0 {
		t.Fatalf("outside: err=%v wrote %d bytes", err, buf.Len())
	}
	link := filepath.Join(n.stateDir, "models", "link.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := n.StreamLargeFile(context.Background(), link, &buf); !errors.Is(err, errSimulatedHost) || buf.Len() != 0 {
		t.Fatalf("link out: err=%v wrote %d bytes", err, buf.Len())
	}
}
