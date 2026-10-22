package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// stringConsts reads the string constants a source file declares, by name, so
// a test can compare two sets of them without listing either by hand.
func stringConsts(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if value, err := strconv.Unquote(lit.Value); err == nil {
					out[name.Name] = value
				}
			}
		}
	}
	return out
}

// storagemount cannot import proto, so it repeats the storage types and
// states the node reports. They must be the same set with the same values, so
// a type or state added on one side cannot be forgotten on the other.
func TestStorageTypesAndStatesMatchProto(t *testing.T) {
	mount := stringConsts(t, "../storagemount/storagemount.go")
	wire := stringConsts(t, "../../proto/storage.go")

	// The constants each side declares for a kind, keyed by the kind's own name
	// (Local, NFSCache, Ready, ...) and holding the string they stand for.
	mountTypes, mountStates, mountReasons := map[string]string{}, map[string]string{}, map[string]string{}
	for name, value := range mount {
		switch {
		case strings.HasPrefix(name, "Reason"):
			mountReasons[strings.TrimPrefix(name, "Reason")] = value
		case strings.HasPrefix(name, "Type"):
			mountTypes[strings.TrimPrefix(name, "Type")] = value
		case strings.HasPrefix(name, "State"):
			mountStates[strings.TrimPrefix(name, "State")] = value
		}
	}
	wireTypes, wireStates, wireReasons := map[string]string{}, map[string]string{}, map[string]string{}
	for name, value := range wire {
		switch {
		case strings.HasPrefix(name, "StorageReason"):
			wireReasons[strings.TrimPrefix(name, "StorageReason")] = value
		case strings.HasPrefix(name, "StorageState"):
			wireStates[strings.TrimPrefix(name, "StorageState")] = value
		case strings.HasPrefix(name, "Storage"):
			wireTypes[strings.TrimPrefix(name, "Storage")] = value
		}
	}
	if len(wireTypes) == 0 || len(wireStates) == 0 {
		t.Fatalf("found no storage types (%v) or states (%v) in proto/storage.go", wireTypes, wireStates)
	}

	compare := func(kind string, mount, wire map[string]string) {
		for name, value := range mount {
			if got, ok := wire[name]; !ok {
				t.Errorf("%s %s = %q exists in storagemount but not in proto", kind, name, value)
			} else if got != value {
				t.Errorf("%s %s: storagemount says %q, proto says %q", kind, name, value, got)
			}
		}
		for name, value := range wire {
			if _, ok := mount[name]; !ok {
				t.Errorf("%s %s = %q exists in proto but not in storagemount", kind, name, value)
			}
		}
	}
	compare("type", mountTypes, wireTypes)
	compare("state", mountStates, wireStates)
	// proto names only the failure reasons the server acts on; each must be a
	// reason storagemount reports, with the same value.
	mountReasonValues := map[string]bool{}
	for _, value := range mountReasons {
		mountReasonValues[value] = true
	}
	for name, value := range wireReasons {
		if !mountReasonValues[value] {
			t.Errorf("reason %s = %q is in proto but storagemount never reports it", name, value)
		}
	}

	// And the values the compiler sees are the ones the source declares.
	for _, pair := range [][2]string{
		{storagemount.TypeLocal, proto.StorageLocal}, {storagemount.TypeNFS, proto.StorageNFS},
		{storagemount.TypeNFSCache, proto.StorageNFSCache}, {storagemount.TypeSharedDir, proto.StorageSharedDir},
		{storagemount.StateReady, proto.StorageStateReady}, {storagemount.StateMounting, proto.StorageStateMounting},
		{storagemount.StateFailed, proto.StorageStateFailed}, {storagemount.StateUnresponsive, proto.StorageStateUnresponsive},
		{storagemount.StateUnsupported, proto.StorageStateUnsupported},
	} {
		if pair[0] != pair[1] {
			t.Errorf("storagemount says %q, proto says %q", pair[0], pair[1])
		}
	}
}
