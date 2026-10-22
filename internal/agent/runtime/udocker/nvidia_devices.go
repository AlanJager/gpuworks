package udocker

import (
	"context"
	"encoding/xml"
	"os"
	"strconv"
	"strings"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
)

// nvidiaDeviceNodeMap resolves a CUDA device index to the /dev/nvidia<N> node
// the driver actually created for that GPU.
//
// The index-named path is NOT always the right one. CUDA enumerates GPUs in the
// order nvidia-smi reports them, but the kernel device node is named after the
// driver's minor number, and in an injected/containerised GPU environment the
// two disagree - e.g. a node exposing /dev/nvidia1,5,6,7 for CUDA indexes 0..3,
// with no /dev/nvidia0 at all. Concatenating "/dev/nvidia"+index then yields a
// path that does not exist, and udocker rejects the whole workload with
// `invalid host volume path`, killing the container before the engine starts.
//
// It is a package variable so tests can substitute a stub.
var nvidiaDeviceNodeMap = cachedNVIDIADeviceNodeMap

// osStat is a seam: tests cannot create /dev/nvidia* nodes.
var osStat = os.Stat

var (
	nvidiaNodesMu       sync.Mutex
	nvidiaNodesResolved bool
	nvidiaNodes         map[int]string
)

// cachedNVIDIADeviceNodeMap keeps the first answer of the driver for the
// process: the GPU set does not change under a running agent, and
// acceleratorMounts runs per workload build. A query that failed (nvidia-smi
// timed out on a busy host) is not kept, so a later build asks again instead
// of falling back to index-named nodes for the agent's whole life.
func cachedNVIDIADeviceNodeMap() map[int]string {
	nvidiaNodesMu.Lock()
	defer nvidiaNodesMu.Unlock()
	if !nvidiaNodesResolved {
		out, err := runNVIDIASMIQueryXML()
		if err != nil {
			return nil
		}
		nvidiaNodes, nvidiaNodesResolved = nodesFromNVIDIAReport(out), true
	}
	return nvidiaNodes
}

// runNVIDIASMIQueryXML is the production source of the driver report. It is
// a read-only query, so it shares the agent's cached vendor queries: on a
// host where nvidia-smi takes a minute the query is not killed at the
// caller's limit and repeated, but finishes once and serves the next build.
var runNVIDIASMIQueryXML = func() ([]byte, error) {
	out, err := gpu.QueryVendorCLI(context.Background(), "nvidia-smi", "-q", "-x")
	return []byte(out), err
}

// deviceNodeExists reports whether a resolved device node is present on this
// host. Only paths that exist may be handed to udocker.
func deviceNodeExists(path string) bool {
	_, err := osStat(path)
	return err == nil
}

// addNVIDIADeviceNode binds the device node backing one CUDA index. Driver
// resolution wins; the historical /dev/nvidia<index> spelling is used only when
// that path actually exists. A node that cannot be resolved either way is
// skipped rather than mounted - a missing bind degrades the container, while a
// nonexistent bind source fails it outright.
func addNVIDIADeviceNode(nodes map[int]string, index string, add func(string)) {
	if n, err := strconv.Atoi(index); err == nil {
		if node, ok := nodes[n]; ok && deviceNodeExists(node) {
			add(node)
			return
		}
	}
	fallback := "/dev/nvidia" + index
	if deviceNodeExists(fallback) {
		add(fallback)
	}
}

// nodesFromNVIDIAReport maps CUDA index -> device node path from the
// driver's own XML report. Returns nil when the report carries no minor
// numbers, so callers fall back to the historical spelling.
func nodesFromNVIDIAReport(out []byte) map[int]string {
	minors := parseNVIDIAMinorNumbers(out)
	if len(minors) == 0 {
		return nil
	}
	nodes := make(map[int]string, len(minors))
	for index, minor := range minors {
		nodes[index] = "/dev/nvidia" + strconv.Itoa(minor)
	}
	return nodes
}

// parseNVIDIAMinorNumbers returns the <minor_number> of every <gpu> in
// `nvidia-smi -q -x`, in document order - which is CUDA index order.
func parseNVIDIAMinorNumbers(out []byte) []int {
	var report struct {
		GPUs []struct {
			MinorNumber string `xml:"minor_number"`
		} `xml:"gpu"`
	}
	if err := xml.Unmarshal(out, &report); err != nil {
		return nil
	}
	minors := make([]int, 0, len(report.GPUs))
	for _, gpu := range report.GPUs {
		minor, err := strconv.Atoi(strings.TrimSpace(gpu.MinorNumber))
		if err != nil {
			// One unparsable minor makes the whole ordering untrustworthy:
			// index N would silently map to the wrong node.
			return nil
		}
		minors = append(minors, minor)
	}
	return minors
}
