package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// errSimulatedHost refuses, on a simulated node, what would reach the real
// host its agent runs on.
var errSimulatedHost = errors.New("not available on a simulated node")

// simulatedNodeRPCs are the RPCs a simulated node answers: what showing the
// product needs (registering a model already on the node, dGPU probing,
// image bookkeeping in its simulated runtime). Anything else (diagnosis,
// probing an evaluation endpoint, storage, network tests, model and image
// transfer, runtime changes) would act on the real host and is refused.
var simulatedNodeRPCs = map[string]bool{
	proto.RPCFSList: true, proto.RPCFSReadFile: true, proto.RPCFSStat: true, proto.RPCRegisterPath: true,
	proto.RPCPartitionReprobe: true, proto.RPCModelCopyStop: true, proto.RPCModelEvict: true, proto.RPCCacheEvict: true,
	proto.RPCAcceleratorPing: true, proto.RPCBenchCancel: true,
	proto.RPCImagePull: true, proto.RPCImageCancel: true, proto.RPCImageRemove: true,
	proto.RPCImageInspect: true, proto.RPCImageExists: true,
}

// pathRPCs name a host path in their "path" parameter; on a simulated node
// it must lie inside the node's own directory.
var pathRPCs = map[string]bool{
	proto.RPCFSList: true, proto.RPCFSReadFile: true, proto.RPCFSStat: true, proto.RPCRegisterPath: true,
}

// simulatedHostRefusal is why a simulated node refuses method, or nil; it is
// always nil on a real node.
func (n *NodeRuntime) simulatedHostRefusal(method string, params json.RawMessage) error {
	if !gpu.SimulatesHost(n.prober) {
		return nil
	}
	if !simulatedNodeRPCs[method] {
		return fmt.Errorf("%s: %w", method, errSimulatedHost)
	}
	if !pathRPCs[method] {
		return nil
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return err
	}
	if !insideDir(n.stateDir, p.Path) {
		return fmt.Errorf("%s outside the node's directory: %w", p.Path, errSimulatedHost)
	}
	return nil
}

// insideDir reports whether path lies inside dir once both are made
// absolute and their symbolic links resolved, so a link cannot lead out.
func insideDir(dir, path string) bool {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) {
		return false
	}
	root, err := resolve(dir)
	if err != nil {
		return false
	}
	target, err := resolve(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve makes path absolute and resolves the links of its longest existing
// prefix: a path that does not exist yet is judged by where it would be.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; cur = filepath.Dir(cur) {
		if _, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			return filepath.Join(real, rest), nil
		}
		if parent := filepath.Dir(cur); parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}
