// Package hostsim is the one policy of an agent that stands in for another
// machine (the experience environment's simulated node): what it describes
// in place of the real host, and what it refuses because it would reach the
// real host its process runs on (a shell, files outside the node's
// directory, ports other than its engines, the network, its own binary).
//
// A real node has no Host: every method is safe on a nil *Host and then
// leaves the agent's behavior exactly as it is, so the agent's own paths
// carry one call each and no knowledge of simulation. Every RPC and stream
// kind is classified here (rules.go), and a test fails when proto grows one
// this package has not classified.
package hostsim

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ErrRefused is what a simulated node answers for anything that would reach
// the real host.
var ErrRefused = errors.New("not available on a simulated node")

// Host is a simulated node's machine. Nil is a real node.
type Host struct {
	// Dir is the node's models directory: the only files it lists, reads
	// or serves. The agent sets it to its own models directory.
	Dir string
	// MayDial reports whether a tunnel may reach addr, a loopback
	// 127.0.0.1:<port> one of the node's engines answers on.
	MayDial func(addr string) bool
	// Describe reports the machine in place of the real host's facts.
	Describe func(proto.SysInfo) proto.SysInfo
	// Samples are the machine's CPU, disk and network figures, in place of
	// the real host's.
	Samples func(node string, now int64) []proto.MetricSample
	// Listeners are the engine ports the machine reports as taken: the
	// other simulated nodes' share of the one host they run on.
	Listeners func(proto.PortRange) []int
	// Libvgpu, when set, stands in for the NVIDIA dGPU library: the node
	// then offers dGPU slicing, and the file it writes is mounted but never
	// loaded by the simulated runtime.
	Libvgpu func(path string) (string, error)
	// MetricsInterval is how often the node collects metrics; zero keeps
	// the agent's own interval.
	MetricsInterval time.Duration
}

// Simulated reports whether h stands for another machine.
func (h *Host) Simulated() bool { return h != nil }

// RPC is nil when the node may serve method with params, else why not.
func (h *Host) RPC(method string, params json.RawMessage) error {
	if h == nil {
		return nil
	}
	switch rpcRules[method] {
	case allow:
		return nil
	case allowInDir:
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return err
		}
		_, err := h.File(p.Path)
		return err
	}
	return fmt.Errorf("%s: %w", method, ErrRefused)
}

// Stream is nil when the node may serve a stream with header hdr.
func (h *Host) Stream(hdr proto.StreamHeader) error {
	if h == nil {
		return nil
	}
	switch streamRules[hdr.Kind] {
	case allow:
		return nil
	case allowInDir:
		// The handler confines the path itself (File), and opens the
		// resolved one.
		return nil
	case allowEngines:
		if h.MayDial != nil && loopbackAddr(hdr.Dst) && h.MayDial(hdr.Dst) {
			return nil
		}
		return fmt.Errorf("tunnel to %s: %w", hdr.Dst, ErrRefused)
	}
	return fmt.Errorf("%s stream: %w", hdr.Kind, ErrRefused)
}

// File is path as the node may open it: unchanged on a real node; on a
// simulated one, absolute, with its links resolved, and inside Dir.
func (h *Host) File(path string) (string, error) {
	if h == nil {
		return path, nil
	}
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || h.Dir == "" {
		return "", fmt.Errorf("%q outside the node's directory: %w", path, ErrRefused)
	}
	root, err := resolve(h.Dir)
	if err != nil {
		return "", fmt.Errorf("%q: %w", path, ErrRefused)
	}
	target, err := resolve(path)
	if err != nil {
		return "", fmt.Errorf("%q: %w", path, ErrRefused)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q outside the node's directory: %w", path, ErrRefused)
	}
	return target, nil
}

// ConfineState drops from a full desired state what would act on the real
// host: a models root or storage outside the node's directory (a shared
// mount, another local path), registry logins, a runtime switch, evaluation
// load (it would leave from the real host). The node's own local storage
// stays, so it reports ready like any node's. Model downloads are returned
// to be failed: the environment serves only its preloaded models.
func (h *Host) ConfineState(ds *proto.DesiredState) (refused []proto.ModelFileTask) {
	if h == nil {
		return nil
	}
	ds.ModelsRoot = h.ownPath(ds.ModelsRoot)
	ds.RegistryAuth, ds.Benchmarks, ds.RuntimePreference = nil, nil, ""
	if ds.Storages != nil {
		kept := []proto.StorageTask{}
		for _, s := range ds.Storages {
			if s.Type == proto.StorageLocal && h.ownPath(s.Path) != "" {
				kept = append(kept, s)
			}
		}
		ds.Storages = kept
	}
	ds.ModelFiles, refused = splitDownloads(ds.ModelFiles)
	return refused
}

// ownPath is path when it lies in the node's directory, else "".
func (h *Host) ownPath(path string) string {
	if _, err := h.File(path); err != nil {
		return ""
	}
	return path
}

// ConfineUpdate is ConfineState for an incremental update.
func (h *Host) ConfineUpdate(su *proto.StateUpdate) (refused []proto.ModelFileTask) {
	if h == nil {
		return nil
	}
	su.ModelsRoot = h.ownPath(su.ModelsRoot)
	su.Benchmarks, su.RemovedBenchmarks = nil, nil
	su.ModelFiles, refused = splitDownloads(su.ModelFiles)
	return refused
}

// splitDownloads keeps withdrawals (which only cancel) and returns the
// tasks that would download.
func splitDownloads(tasks []proto.ModelFileTask) (kept, downloads []proto.ModelFileTask) {
	for _, mf := range tasks {
		if mf.DesiredState == "present" {
			downloads = append(downloads, mf)
		} else {
			kept = append(kept, mf)
		}
	}
	return kept, downloads
}

// Capabilities drops what a simulated node refuses from the capabilities it
// would advertise, so the console does not offer it.
func (h *Host) Capabilities(caps []string) []string {
	if h == nil {
		return caps
	}
	out := caps[:0:0]
	for _, c := range caps {
		if !refusedCaps[c] {
			out = append(out, c)
		}
	}
	return out
}

// SysInfo is sys as the node reports it.
func (h *Host) SysInfo(sys proto.SysInfo) proto.SysInfo {
	if h == nil || h.Describe == nil {
		return sys
	}
	return h.Describe(sys)
}

// HostSamples are the machine's host figures; ok is false where the agent
// samples the real host.
func (h *Host) HostSamples(node string, now int64) (samples []proto.MetricSample, ok bool) {
	if h == nil || h.Samples == nil {
		return nil, false
	}
	return h.Samples(node, now), true
}

// EnginePortListeners are the engine ports the node reports taken; ok is
// false where the agent scans the real host.
func (h *Host) EnginePortListeners(r proto.PortRange) (ports []int, ok bool) {
	if h == nil || h.Listeners == nil {
		return nil, false
	}
	return h.Listeners(r), true
}

// ShipsHostLogs reports whether the node ships the host's logs: a simulated
// node's host is not the machine it stands for.
func (h *Host) ShipsHostLogs() bool { return h == nil }

// PullsImages reports whether an image pull asks a registry; a simulated
// node has every image, and asking would reach the network.
func (h *Host) PullsImages() bool { return h == nil }

// LibvgpuExtractor is the dGPU library's stand-in, or nil.
func (h *Host) LibvgpuExtractor() func(string) (string, error) {
	if h == nil {
		return nil
	}
	return h.Libvgpu
}

// CollectInterval is how often the node collects metrics.
func (h *Host) CollectInterval(def time.Duration) time.Duration {
	if h == nil || h.MetricsInterval <= 0 {
		return def
	}
	return h.MetricsInterval
}

// loopbackAddr accepts only 127.0.0.1:<port>: "localhost" may resolve to
// ::1, where the engine host does not listen and another program may.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && host == "127.0.0.1"
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
