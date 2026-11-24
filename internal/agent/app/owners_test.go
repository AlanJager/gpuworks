package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/proto/prototest"
)

// The tables in this file say who owns each way into the agent and each
// field of NodeRuntime (ADR 0066, decision 8). The tests fail on an entry or
// a field that has no owner.

type owner string

const (
	ownImages    owner = "images"
	ownModels    owner = "models"
	ownStorage   owner = "storage"
	ownInstances owner = "instances"
	ownCards     owner = "cards"
	ownDoctor    owner = "doctor"
	ownRuntime   owner = "runtime"
	ownNetwork   owner = "network"
	ownBench     owner = "bench"
	ownLogs      owner = "logs"
	ownPlatform  owner = "platform"
	ownRelay     owner = "relay"
	ownShared    owner = "shared"
	ownWiring    owner = "wiring"
	ownElsewhere owner = "elsewhere"
)

// owners is every owner; a table may name no other. The text says what the
// owner covers and is read by people only.
var owners = map[owner]string{
	ownImages:    "catalog images on the node: pull, load, copy, peer transfer",
	ownModels:    "model files: download, copy, sync, eviction",
	ownStorage:   "the storages a node mounts, and files on the node read by path",
	ownInstances: "the engine: instances, their containers, terminals and tunnels into them",
	ownCards:     "accelerator cards: health, checks, occupancy, dGPU probing, the vendor managed",
	ownDoctor:    "the doctor report and the checks it runs",
	ownRuntime:   "which container runtime the node uses, whether it can use the cards, its proxy",
	ownNetwork:   "probes and benchmarks of the node's network",
	ownBench:     "benchmark runs against a served model",
	ownLogs:      "instance, node and agent logs, debug sessions",
	ownPlatform:  "the allocator a node is a guest of: Kubernetes or Slurm",
	ownRelay:     "the relayed connection between two nodes' transfers; the payload is its own owner's",

	ownShared:    "what several owners read and the composition root holds: ADR 0066 decision 5",
	ownWiring:    "the composition root's own: the connection and what waits to be sent on it, the process's identity and directories, injected dependencies",
	ownElsewhere: "not served by package app: package conn answers it, or the agent sends it",
}

// extraction is how an owner that has a type of its own is held and reached.
type extraction struct {
	field    string                   // its one NodeRuntime field
	accessor string                   // the only function that names that field
	prefix   string                   // what the wire name of each of its RPCs starts with
	serves   func(method string) bool // whether its handler table has an RPC
}

// extracted is the owners that have a type of their own.
var extracted = map[owner]extraction{
	ownImages: {field: "images", accessor: "imageWork", prefix: "image.", serves: func(method string) bool { return (&nodeImages{}).handler(method) != nil }},
}

// rpcOwners is the owner of every RPC that no extracted owner's table serves.
var rpcOwners = map[string]owner{
	proto.RPCModelDownloadStop: ownModels,
	proto.RPCModelCopyStop:     ownModels,
	proto.RPCModelEvict:        ownModels,
	proto.RPCCacheEvict:        ownModels,
	proto.RPCRegisterPath:      ownModels,
	proto.RPCSourceProbe:       ownModels,
	proto.RPCModelSyncListen:   ownModels,
	proto.RPCModelSyncSend:     ownModels,
	proto.RPCModelSyncStop:     ownModels,
	proto.RPCModelSyncProbe:    ownModels,

	proto.RPCStorageProbe:       ownStorage,
	proto.RPCStorageScan:        ownStorage,
	proto.RPCStoragePeers:       ownStorage,
	proto.RPCStorageRemove:      ownStorage,
	proto.RPCStorageFilesList:   ownStorage,
	proto.RPCStorageFilesMkdir:  ownStorage,
	proto.RPCStorageFilesRename: ownStorage,
	proto.RPCStorageFilesDelete: ownStorage,
	// Any path on the node, for model registration, storage scans and
	// benchmark tokenizers alike.
	proto.RPCFSList:     ownStorage,
	proto.RPCFSReadFile: ownStorage,
	proto.RPCFSStat:     ownStorage,

	proto.RPCNodeCardCheckDetail: ownCards,
	proto.RPCNodeCardRecheck:     ownCards,
	proto.RPCNodeSetAccelerator:  ownCards,
	proto.RPCPartitionReprobe:    ownCards,

	proto.RPCNodeDoctor: ownDoctor,

	proto.RPCNodeSetRuntime: ownRuntime,
	proto.RPCRuntimeRestart: ownRuntime,

	proto.RPCNetProbe:        ownNetwork,
	proto.RPCNetBenchStart:   ownNetwork,
	proto.RPCNetBenchStop:    ownNetwork,
	proto.RPCRDMABenchServe:  ownNetwork,
	proto.RPCRDMABenchRun:    ownNetwork,
	proto.RPCAcceleratorPing: ownNetwork, // from a card's network port

	proto.RPCBenchProbe:  ownBench,
	proto.RPCBenchCancel: ownBench,

	proto.RPCDebugStart: ownLogs,
	proto.RPCDebugStop:  ownLogs,

	proto.RPCKubeNodeExists:    ownPlatform,
	proto.RPCKubeNodesSelected: ownPlatform,

	proto.RPCPeerRelayWaitClosed: ownRelay,

	// Answered by package conn.
	proto.RPCNodeConnectionDiagnostics: ownElsewhere,
	proto.RPCAgentUpgrade:              ownElsewhere,
	// Asked by the agent, answered by the server.
	proto.RPCNodeCapabilities:  ownElsewhere,
	proto.RPCObservationCancel: ownElsewhere,
	proto.RPCObservationRenew:  ownElsewhere,
	proto.RPCNetBenchRenew:     ownElsewhere,
}

// streamOwners is the owner of every stream kind.
var streamOwners = map[string]owner{
	proto.StreamImageRead:        ownImages,
	proto.StreamImageWrite:       ownImages,
	proto.StreamModelRead:        ownModels,
	proto.StreamModelWrite:       ownModels,
	proto.StreamLargeFileRead:    ownStorage, // one file by path, as the fs RPCs read
	proto.StreamStorageUpload:    ownStorage,
	proto.StreamStorageDownload:  ownStorage,
	proto.StreamTunnel:           ownInstances,
	proto.StreamTerminal:         ownInstances,
	proto.StreamLogtail:          ownLogs,
	proto.StreamDiagnosticsRead:  ownLogs,
	proto.StreamPeerRelaySend:    ownRelay,
	proto.StreamPeerRelayReceive: ownRelay,
	proto.StreamCtrl:             ownElsewhere,
	proto.StreamAgentUpgrade:     ownElsewhere,
	proto.StreamTelemetry:        ownElsewhere,
}

// stateOwners is the owner of every field the server syncs
// (proto.DesiredState and proto.StateUpdate).
var stateOwners = map[string]owner{
	"Instances": ownInstances, "RemovedInstances": ownInstances, "Observations": ownInstances,
	"OrphanCleanup": ownInstances, "ReleasedDirGroups": ownInstances, "ReleasedDirDeployments": ownInstances,
	"ModelFiles": ownModels, "RemovedModelFiles": ownModels,
	"Benchmarks": ownBench, "RemovedBenchmarks": ownBench,
	"ImageRefs":             ownImages,
	"Storages":              ownStorage,
	"RuntimePreference":     ownRuntime,
	"AcceleratorPreference": ownCards,
	"Network":               ownShared,
	"RegistryAuth":          ownShared,
	"ModelsRoot":            ownShared,
}

// methodOwners is the owner of every exported NodeRuntime method: what
// package conn and the agent's main call, including the facts a status
// report asks the node for.
var methodOwners = map[string]owner{
	"StreamImage": ownImages,

	"StreamModelArchive": ownModels, "ReceiveModelArchive": ownModels,

	"ReceiveStorageUpload": ownStorage, "SendStorageDownload": ownStorage, "StreamLargeFile": ownStorage,

	"HelloInventory": ownInstances, "ContainerTerminalArgv": ownInstances,
	"ContainerTerminalCapabilities": ownInstances, "ContainerTerminalSession": ownInstances,

	"AcceleratorVendors": ownCards, "SetAccelerator": ownCards, "CardHealthFindings": ownCards,
	"PartitionAssets": ownCards, "PartitionProbes": ownCards, "CachedPartitionProbes": ownCards,
	"PeerMemory": ownCards,

	"RuntimeDriverName": ownRuntime, "ApplyRuntimePreference": ownRuntime, "SwitchRuntime": ownRuntime,
	"RuntimeFallback": ownRuntime, "RuntimeGPU": ownRuntime, "RuntimeProxyStatus": ownRuntime,

	"NetworkTools": ownNetwork, "UCXNetlinkHang": ownNetwork,

	"StreamLogs": ownLogs, "StreamDiagnostics": ownLogs,

	"OnKubeSettings": ownPlatform, "SchedulerState": ownPlatform, "NVIDIAIsolation": ownPlatform,

	"StreamPeerSend": ownRelay, "PeerRelayReceiver": ownRelay,

	"ModelsRoot": ownShared,

	"Bind": ownWiring, "Unbind": ownWiring, "SetNodeToken": ownWiring, "StartBackground": ownWiring,
	"OwnModelsDir": ownWiring,
	// These hand each part of what arrives to its owner.
	"OnRPC": ownWiring, "OnStateSync": ownWiring, "OnStateUpdate": ownWiring, "StatusSnapshot": ownWiring,
}

// fieldOwners is the owner of every NodeRuntime field. An owner without a
// type of its own still has its state here, field by field.
var fieldOwners = map[string]owner{
	"images": ownImages,

	"dlMu": ownModels, "downloading": ownModels, "stoppedCopies": ownModels, "stoppedDownloads": ownModels,
	"syncMu": ownModels, "modelSyncs": ownModels,

	"storage": ownStorage,

	"engine": ownInstances, "servingForward": ownInstances,

	"cardSmoke": ownCards, "cardSmokeCtx": ownCards, "checks": ownCards, "pcie": ownCards, "nvlink": ownCards,
	"xid": ownCards, "occupancy": ownCards, "accelerator": ownCards, "dcgmMode": ownCards,
	"instanceCardIdents": ownCards, "heldCards": ownCards, "sleepingCards": ownCards,
	"partitionProbe": ownCards, "partitionProbeStateMu": ownCards,
	"partitionProbeInputsSeen": ownCards, "partitionProbeFingerprintSeen": ownCards,

	"doctorRunning": ownDoctor,

	"switchMu": ownRuntime, "runtimeChoice": ownRuntime, "runtimeProxy": ownRuntime, "runtimeGPU": ownRuntime,

	"netBench": ownNetwork, "perftest": ownNetwork, "ucxNetlinkOnce": ownNetwork, "ucxNetlink": ownNetwork,

	"bench": ownBench,

	"instanceLogs": ownLogs, "logOutOnce": ownLogs, "logOut": ownLogs, "logFollowerRestarts": ownLogs,
	"logNoiseDropped": ownLogs, "logNodeDropped": ownLogs, "journal": ownLogs, "selfLog": ownLogs,
	"debugLog": ownLogs,

	"kubeCordon": ownPlatform,

	"modelsDirMu": ownShared, "modelsDir": ownShared, "netMu": ownShared, "netcfg": ownShared,
	"authMu": ownShared, "registryAuth": ownShared, "puller": ownShared, "registries": ownShared,

	"log": ownWiring, "prober": ownWiring, "host": ownWiring, "kubeNode": ownWiring,
	"stateDir": ownWiring, "logDir": ownWiring, "serverURL": ownWiring, "nodeToken": ownWiring,
	"sendMu": ownWiring, "send": ownWiring, "observationConnectionEpoch": ownWiring, "benchPending": ownWiring,
	"progress": ownWiring, "collectorRunner": ownWiring,
}

// checkOwner fails when o is not an owner, or is one of the kinds the table
// may not use.
func checkOwner(t *testing.T, table, key string, o owner, not ...owner) {
	t.Helper()
	if _, ok := owners[o]; !ok {
		t.Errorf("%s[%s] = %q, which is not an owner", table, key, o)
	}
	for _, refused := range not {
		if o == refused {
			t.Errorf("%s[%s] = %s, which this table may not name", table, key, o)
		}
	}
}

func protoConstants(t *testing.T, prefix string) map[string]string {
	return prototest.Constants(t, filepath.Join("..", "..", "proto"), prefix)
}

// An RPC is served by one extracted owner's table, or it has a line in
// rpcOwners naming an owner without a table. Never both, never neither. A
// table serves only RPCs whose wire name starts with its owner's prefix.
func TestEveryRPCHasOneOwner(t *testing.T) {
	for name, method := range protoConstants(t, "RPC") {
		var serving []string
		for o, x := range extracted {
			if !x.serves(method) {
				continue
			}
			serving = append(serving, string(o))
			if !strings.HasPrefix(method, x.prefix) {
				t.Errorf("proto.%s (%q) is served by %s's table, whose RPCs start with %q", name, method, o, x.prefix)
			}
		}
		listed, isListed := rpcOwners[method]
		switch {
		case len(serving) > 1:
			sort.Strings(serving)
			t.Errorf("proto.%s (%q) is served by the tables of %v", name, method, serving)
		case len(serving) == 1 && isListed:
			t.Errorf("proto.%s (%q) is served by %s's table and also listed in rpcOwners as %s", name, method, serving[0], listed)
		case len(serving) == 0 && !isListed:
			t.Errorf("proto.%s (%q) has no owner: add its handler to its owner's table, or a line to rpcOwners when the owner has none", name, method)
		}
	}
	for method, o := range rpcOwners {
		checkOwner(t, "rpcOwners", method, o, ownShared, ownWiring)
		if _, ok := extracted[o]; ok {
			t.Errorf("rpcOwners[%s] = %s, which has a handler table: add the handler there", method, o)
		}
	}
}

func TestEveryStreamHasAnOwner(t *testing.T) {
	for name, kind := range protoConstants(t, "Stream") {
		if _, ok := streamOwners[kind]; !ok {
			t.Errorf("proto.%s (%q) has no owner in streamOwners", name, kind)
		}
	}
	for kind, o := range streamOwners {
		checkOwner(t, "streamOwners", kind, o, ownShared, ownWiring)
	}
}

func TestEverySyncedFieldHasAnOwner(t *testing.T) {
	seen := map[string]bool{}
	for _, ty := range []reflect.Type{reflect.TypeOf(proto.DesiredState{}), reflect.TypeOf(proto.StateUpdate{})} {
		for i := 0; i < ty.NumField(); i++ {
			name := ty.Field(i).Name
			seen[name] = true
			if _, ok := stateOwners[name]; !ok {
				t.Errorf("proto.%s.%s has no owner in stateOwners", ty.Name(), name)
			}
		}
	}
	for name, o := range stateOwners {
		if !seen[name] {
			t.Errorf("stateOwners lists %s, which the server no longer syncs", name)
		}
		checkOwner(t, "stateOwners", name, o, ownWiring, ownElsewhere)
	}
}

func TestEveryExportedNodeRuntimeMethodHasAnOwner(t *testing.T) {
	ty := reflect.TypeOf(&NodeRuntime{})
	seen := map[string]bool{}
	for i := 0; i < ty.NumMethod(); i++ {
		name := ty.Method(i).Name
		seen[name] = true
		if _, ok := methodOwners[name]; !ok {
			t.Errorf("NodeRuntime.%s has no owner in methodOwners", name)
		}
	}
	for name, o := range methodOwners {
		if !seen[name] {
			t.Errorf("methodOwners lists %s, which NodeRuntime no longer has", name)
		}
		checkOwner(t, "methodOwners", name, o, ownElsewhere)
	}
}

// An extracted owner is exactly one NodeRuntime field, the one it declares.
func TestEveryNodeRuntimeFieldHasAnOwner(t *testing.T) {
	ty := reflect.TypeOf(NodeRuntime{})
	perOwner := map[owner][]string{}
	seen := map[string]bool{}
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		seen[name] = true
		o, ok := fieldOwners[name]
		if !ok {
			t.Errorf("NodeRuntime.%s has no owner in fieldOwners", name)
			continue
		}
		perOwner[o] = append(perOwner[o], name)
	}
	for name, o := range fieldOwners {
		if !seen[name] {
			t.Errorf("fieldOwners lists %s, which NodeRuntime no longer has", name)
		}
		checkOwner(t, "fieldOwners", name, o, ownElsewhere)
	}
	for o, x := range extracted {
		if fields := perOwner[o]; len(fields) != 1 || fields[0] != x.field {
			sort.Strings(fields)
			t.Errorf("%s has NodeRuntime fields %v, want only %s", o, fields, x.field)
		}
	}
}

// appFiles is the package's non-test files, parsed.
func appFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info os.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		t.Fatal("no source files parsed")
	}
	return fset, files
}

// mentions reports whether a type expression names typ.
func mentions(expr ast.Expr, typ string) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == typ {
			found = true
		}
		return !found
	})
	return found
}

// Only an extracted owner's accessor names its NodeRuntime field, so no code
// of the node reads the owner's state or takes its locks. The field is
// looked for on a receiver or parameter of the node's type; reached through
// a local variable or an embedding type it is not seen here.
func TestAnExtractedOwnerIsReachedOnlyThroughItsAccessor(t *testing.T) {
	accessorOf := map[string]string{}
	for _, x := range extracted {
		accessorOf[x.field] = x.accessor
	}
	fset, files := appFiles(t)
	accessors := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			nodes := map[string]bool{}
			for _, list := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
				if list == nil {
					continue
				}
				for _, field := range list.List {
					if mentions(field.Type, "NodeRuntime") {
						for _, name := range field.Names {
							nodes[name.Name] = true
						}
					}
				}
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				sel, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok || !nodes[x.Name] {
					return true
				}
				accessor, ok := accessorOf[sel.Sel.Name]
				if !ok {
					return true
				}
				if fn.Name.Name == accessor {
					accessors++
					return true
				}
				t.Errorf("%s: %s.%s is named outside %s; call %s() and ask through a method", fset.Position(sel.Pos()), x.Name, sel.Sel.Name, accessor, accessor)
				return true
			})
		}
	}
	if accessors == 0 {
		t.Fatal("no accessor names its field: the check inspected nothing")
	}
}

// No struct holds the node: an owner gets the functions it needs, not
// NodeRuntime. simulatedNetwork is the node itself as package conn sees it
// on a simulated host.
func TestNoTypeHoldsTheNode(t *testing.T) {
	fset, files := appFiles(t)
	checked := 0
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			if spec, ok := node.(*ast.TypeSpec); ok && spec.Name.Name == "simulatedNetwork" {
				return false
			}
			st, ok := node.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				checked++
				if mentions(field.Type, "NodeRuntime") {
					t.Errorf("%s: a struct field's type names NodeRuntime", fset.Position(field.Pos()))
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no struct fields were inspected")
	}
}

// isLock reports whether a type expression is a pointer to a sync mutex or a
// sync.Locker.
func isLock(expr ast.Expr) bool {
	pointer := false
	if star, ok := expr.(*ast.StarExpr); ok {
		expr, pointer = star.X, true
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "sync" {
		return false
	}
	if pointer {
		return strings.HasSuffix(sel.Sel.Name, "Mutex")
	}
	return sel.Sel.Name == "Locker"
}

// No struct field and no function parameter is a pointer to a mutex or a
// sync.Locker: that is how one type uses a lock another owns. A lock reached
// some other way (a closure, a pointer to the struct that holds it) is not
// seen here.
func TestNoLockIsPassedAround(t *testing.T) {
	fset, files := appFiles(t)
	checked := 0
	check := func(list *ast.FieldList, what string) {
		if list == nil {
			return
		}
		for _, field := range list.List {
			checked++
			if isLock(field.Type) {
				t.Errorf("%s: a %s is a lock; the type that owns the state keeps the lock and answers through a method", fset.Position(field.Pos()), what)
			}
		}
	}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.StructType:
				check(n.Fields, "struct field")
			case *ast.FuncType:
				check(n.Params, "parameter")
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("nothing was inspected")
	}
}
