package hostsim

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// protoConstants are the string constants of package proto whose names
// start with prefix, read from its source so a new one is seen here the day
// it is added.
func protoConstants(t *testing.T, prefix string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	dir := filepath.Join("..", "..", "proto")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if !strings.HasPrefix(name.Name, prefix) || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, _ := strconv.Unquote(lit.Value)
					out[name.Name] = value
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s constants found in package proto", prefix)
	}
	return out
}

// TestEveryRPCIsClassified makes a new RPC decide, here, whether a simulated
// node serves it: unclassified it is refused, which a demo would show as a
// broken feature rather than a host reached.
func TestEveryRPCIsClassified(t *testing.T) {
	for name, method := range protoConstants(t, "RPC") {
		if _, ok := rpcRules[method]; !ok {
			t.Errorf("proto.%s (%q) is not classified in hostsim.rpcRules", name, method)
		}
	}
}

func TestEveryStreamIsClassified(t *testing.T) {
	for name, kind := range protoConstants(t, "Stream") {
		if _, ok := streamRules[kind]; !ok {
			t.Errorf("proto.%s (%q) is not classified in hostsim.streamRules", name, kind)
		}
	}
}

func TestNilHostIsARealNode(t *testing.T) {
	var h *Host
	if h.Simulated() || !h.ShipsHostLogs() || !h.PullsImages() || h.LibvgpuExtractor() != nil {
		t.Fatal("a nil host must leave the agent as it is")
	}
	for _, method := range []string{proto.RPCNodeDoctor, proto.RPCAgentUpgrade, "no.such.rpc"} {
		if err := h.RPC(method, nil); err != nil {
			t.Fatalf("real node %s refused: %v", method, err)
		}
	}
	if err := h.Stream(proto.StreamHeader{Kind: proto.StreamTerminal}); err != nil {
		t.Fatalf("real node terminal refused: %v", err)
	}
	if p, err := h.File("relative/path"); err != nil || p != "relative/path" {
		t.Fatalf("real node file = %q, %v", p, err)
	}
	ds := proto.DesiredState{ModelsRoot: "/data", Benchmarks: []proto.BenchmarkSpec{{}}, ModelFiles: []proto.ModelFileTask{{DesiredState: "present"}}}
	if refused := h.ConfineState(&ds); refused != nil || ds.ModelsRoot != "/data" || len(ds.Benchmarks) != 1 || len(ds.ModelFiles) != 1 {
		t.Fatalf("real node desired state changed: %+v refused %v", ds, refused)
	}
	caps := []string{proto.CapWebterm, proto.CapBench}
	if got := h.Capabilities(caps); len(got) != 2 {
		t.Fatalf("real node caps = %v", got)
	}
	if _, ok := h.HostSamples("n", 0); ok {
		t.Fatal("real node samples the real host")
	}
	if _, ok := h.EnginePortListeners(proto.PortRange{Start: 1, End: 2}); ok {
		t.Fatal("real node scans the real host")
	}
	if got := h.CollectInterval(7); got != 7 {
		t.Fatalf("interval = %v", got)
	}
}

// simulated is a simulated node whose directory holds a model file, and a
// file outside it to reach for.
func simulated(t *testing.T) (h *Host, inside, outside string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "node", "models")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	inside = filepath.Join(dir, "tokenizer.json")
	outside = filepath.Join(root, "secret.txt")
	for path, data := range map[string]string{inside: `{"model":{}}`, outside: "host secret"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h = &Host{Dir: dir, MayDial: func(addr string) bool { return addr == "127.0.0.1:30001" }}
	return h, inside, outside
}

func pathParams(path string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"path": path})
	return raw
}

func TestSimulatedNodeServesOnlyAllowedRPCs(t *testing.T) {
	h, inside, _ := simulated(t)
	for method, r := range rpcRules {
		params := json.RawMessage(`{}`)
		if r == allowInDir {
			params = pathParams(inside)
		}
		err := h.RPC(method, params)
		if r == refuse && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want refused", method, err)
		}
		if r != refuse && err != nil {
			t.Errorf("%s refused: %v", method, err)
		}
	}
	if err := h.RPC("no.such.rpc", nil); !errors.Is(err, ErrRefused) {
		t.Fatalf("unknown rpc: err = %v", err)
	}
}

func TestSimulatedNodeConfinesPathsToItsDirectory(t *testing.T) {
	h, inside, outside := simulated(t)
	escape := filepath.Join(h.Dir, "escape")
	if err := os.Symlink(filepath.Dir(outside), escape); err != nil {
		t.Fatal(err)
	}
	for path, ok := range map[string]bool{
		inside:                               true,
		filepath.Join(h.Dir, "not-yet", "x"): true, // judged by where it would be
		h.Dir:                                true,
		outside:                              false,
		"/etc/passwd":                        false,
		filepath.Join(h.Dir, ".."):           false, // the state directory, with the node's credentials
		filepath.Join(escape, "secret.txt"):  false, // a link that leads out
		"models/tokenizer.json":              false, // relative
		"":                                   false,
	} {
		for method, r := range rpcRules {
			if r != allowInDir {
				continue
			}
			err := h.RPC(method, pathParams(path))
			if ok && err != nil {
				t.Errorf("%s %q refused: %v", method, path, err)
			}
			if !ok && !errors.Is(err, ErrRefused) {
				t.Errorf("%s %q: err = %v, want refused", method, path, err)
			}
		}
	}
	if err := h.RPC(proto.RPCFSReadFile, json.RawMessage(`{"path":`)); err == nil {
		t.Fatal("malformed params accepted")
	}
	// File hands back the resolved path, which the caller opens: a link
	// swapped in afterwards cannot redirect it.
	link := filepath.Join(h.Dir, "link.json")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	got, err := h.File(link)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(inside); got != want {
		t.Fatalf("File(link) = %q, want %q", got, want)
	}
}

func TestSimulatedNodeStreams(t *testing.T) {
	h, _, _ := simulated(t)
	for kind, r := range streamRules {
		hdr := proto.StreamHeader{Kind: kind, Dst: "127.0.0.1:30001"}
		err := h.Stream(hdr)
		if r == refuse && !errors.Is(err, ErrRefused) {
			t.Errorf("%s: err = %v, want refused", kind, err)
		}
		if r != refuse && err != nil {
			t.Errorf("%s refused: %v", kind, err)
		}
	}
	for _, dst := range []string{"127.0.0.1:30002", "localhost:30001", "[::1]:30001", "10.0.0.1:30001", ""} {
		if err := h.Stream(proto.StreamHeader{Kind: proto.StreamTunnel, Dst: dst}); !errors.Is(err, ErrRefused) {
			t.Errorf("tunnel to %q: err = %v, want refused", dst, err)
		}
	}
	if err := h.Stream(proto.StreamHeader{Kind: "no.such.stream"}); !errors.Is(err, ErrRefused) {
		t.Fatalf("unknown stream: err = %v", err)
	}
}

func TestSimulatedNodeConfinesDesiredState(t *testing.T) {
	h, _, _ := simulated(t)
	download := proto.ModelFileTask{Key: "a", DesiredState: "present"}
	withdraw := proto.ModelFileTask{Key: "b", DesiredState: "absent"}
	own := proto.StorageTask{ID: 1, Type: proto.StorageLocal, Path: h.Dir}
	ds := proto.DesiredState{
		ModelsRoot: "/", RegistryAuth: []proto.SourceAuth{{Host: "r"}},
		Storages: []proto.StorageTask{
			own,
			{ID: 2, Type: proto.StorageLocal, Path: "/srv/models"},
			{ID: 3, Type: proto.StorageNFS, Path: filepath.Join(h.Dir, "nfs"), Export: "10.0.0.1:/x"},
		},
		Benchmarks: []proto.BenchmarkSpec{{}}, ModelFiles: []proto.ModelFileTask{download, withdraw},
		Instances: []proto.InstanceDesiredState{{UUID: "i"}},
	}
	refused := h.ConfineState(&ds)
	if ds.ModelsRoot != "" || ds.RegistryAuth != nil || ds.Benchmarks != nil {
		t.Fatalf("host-reaching fields kept: %+v", ds)
	}
	// The node's own local storage stays and reports like any node's; a
	// mount or another path does not.
	if len(ds.Storages) != 1 || ds.Storages[0].ID != 1 {
		t.Fatalf("storages = %+v, want only the node's own", ds.Storages)
	}
	inside := proto.DesiredState{ModelsRoot: h.Dir}
	h.ConfineState(&inside)
	if inside.ModelsRoot != h.Dir {
		t.Fatalf("the node's own models root dropped: %q", inside.ModelsRoot)
	}
	if len(ds.Instances) != 1 || len(ds.ModelFiles) != 1 || ds.ModelFiles[0].Key != "b" {
		t.Fatalf("kept = %+v", ds)
	}
	if len(refused) != 1 || refused[0].Key != "a" {
		t.Fatalf("refused = %+v", refused)
	}
	su := proto.StateUpdate{ModelsRoot: "/", Benchmarks: []proto.BenchmarkSpec{{}}, RemovedBenchmarks: []uint{1}, ModelFiles: []proto.ModelFileTask{download}}
	if refused := h.ConfineUpdate(&su); len(refused) != 1 || su.ModelsRoot != "" || su.Benchmarks != nil || su.RemovedBenchmarks != nil || len(su.ModelFiles) != 0 {
		t.Fatalf("update = %+v refused %v", su, refused)
	}
}

func TestSimulatedNodeCapabilities(t *testing.T) {
	h, _, _ := simulated(t)
	got := h.Capabilities([]string{proto.CapWebterm, proto.CapUpgradePush, proto.CapImagePullCandidates, proto.CapBench, proto.CapNetworkUpdate})
	if len(got) != 2 || got[0] != proto.CapImagePullCandidates || got[1] != proto.CapNetworkUpdate {
		t.Fatalf("caps = %v", got)
	}
}
