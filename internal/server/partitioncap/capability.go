// Package partitioncap derives partition capabilities from backend policy and
// node-observed facts. It is the only place that creates status/reason text.
package partitioncap

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	StatusReady       = "ready"
	StatusUnavailable = "unavailable"
)

type unavailableKind int

const (
	unavailableOldAgent unavailableKind = iota + 1
	unavailableMissingAssets
	unavailableRuntime
	unavailableProbePending
	unavailableProbeFailed
)

// reason is intentionally the single source of all unavailable wording.
func reason(kind unavailableKind, detail string) string {
	switch kind {
	case unavailableOldAgent:
		return "agent has not reported current dGPU safety capability; upgrade the agent"
	case unavailableMissingAssets:
		return "missing dGPU assets: " + detail
	case unavailableRuntime:
		if strings.TrimSpace(detail) == "" {
			return "container inference unverified"
		}
		return "container inference unverified: " + detail
	case unavailableProbePending:
		if strings.TrimSpace(detail) == "" {
			return "dGPU probe has not completed"
		}
		return "dGPU probe has not completed: " + detail
	case unavailableProbeFailed:
		if strings.TrimSpace(detail) == "" {
			return "dGPU probe failed"
		}
		return "dGPU probe failed: " + detail
	default:
		return "dGPU unavailable"
	}
}

// Reports is a node's partition asset and probe reports decoded once, so the
// capability of each of its cards is derived without decoding them per card:
// the scheduler's node snapshot and the GPU inventory walk every card of every
// node.
type Reports struct {
	node          *store.Node
	assets        map[string][]proto.AssetReport
	assetsPresent bool
	probes        map[probeKey]proto.PartitionProbeResult
	legacyProbes  map[string]bool
	probesPresent bool
}

// Decode reads node's partition reports once.
func Decode(node *store.Node) Reports {
	if node == nil {
		return Reports{}
	}
	r := Reports{node: node}
	r.assets, r.assetsPresent = assetReports(node.PartitionAssets)
	r.probes, r.legacyProbes, r.probesPresent = probeReports(node.PartitionProbes)
	return r
}

func ForGPU(node *store.Node, gpu *store.GPU) []proto.PartitionCapability {
	return Decode(node).ForGPU(gpu)
}

// ForGPU derives the capabilities of one of the node's cards.
func (r Reports) ForGPU(gpu *store.GPU) []proto.PartitionCapability {
	node := r.node
	if node == nil || gpu == nil {
		return nil
	}
	vendor := accelerator.NormalizeVendor(gpu.Vendor, node.GPUVendor)
	reports, reportsPresent := r.assets, r.assetsPresent
	probes, legacyProbes, probesPresent := r.probes, r.legacyProbes, r.probesPresent
	var out []proto.PartitionCapability
	for _, backend := range accelerator.All() {
		if !backend.Matches(vendor, gpu.ArchID) {
			continue
		}
		meta := backend.Metadata()
		capability := proto.PartitionCapability{
			Backend: backend.ID(), DisplayName: meta.DisplayName, Vendor: meta.Vendor,
			Maturity: string(backend.Maturity()), Status: StatusUnavailable,
			Compatibility: meta.Compatibility,
		}
		if strings.TrimSpace(node.RuntimeDriver) == "" {
			capability.Reason = reason(unavailableRuntime, "no usable docker/udocker runtime on the node")
			out = append(out, capability)
			continue
		}
		if why := accelerator.RuntimeConstraint(backend, node.RuntimeDriver); why != "" {
			capability.Reason = reason(unavailableRuntime, why)
			out = append(out, capability)
			continue
		}
		requiresProbe := !accelerator.SkipsBehaviorProbe(backend)
		if !requiresProbe && node.PartitionSafetyProtocol < 1 {
			capability.Reason = reason(unavailableOldAgent, "")
			out = append(out, capability)
			continue
		}
		if requiresProbe {
			capability.CompatibilityNotes = compatibilityNotes(backend, node, gpu)
			if !probesPresent {
				capability.Reason = reason(unavailableOldAgent, "")
				out = append(out, capability)
				continue
			}
		}
		if !reportsPresent {
			capability.Reason = reason(unavailableMissingAssets, strings.Join(backend.RequiredAssets(), ", "))
			out = append(out, capability)
			continue
		}
		missing := missingAssets(backend.RequiredAssets(), reports[backend.ID()])
		if len(missing) > 0 {
			capability.Reason = reason(unavailableMissingAssets, strings.Join(missing, ", "))
			out = append(out, capability)
			continue
		}
		if requiresProbe {
			probe, ok := probes[probeKey{backend: backend.ID(), gpuIndex: gpu.Index}]
			if !ok {
				if legacyProbes[backend.ID()] {
					capability.Reason = reason(unavailableOldAgent, "")
				} else {
					capability.Reason = reason(unavailableProbePending, fmt.Sprintf("no result received for GPU %d", gpu.Index))
				}
				out = append(out, capability)
				continue
			}
			capability.ProbeStatus = probe.Status
			if !probe.CheckedAt.IsZero() {
				checkedAt := probe.CheckedAt
				capability.ProbeCheckedAt = &checkedAt
			}
			switch probe.Status {
			case proto.PartitionProbePassed:
				// Ready is assigned below after every factual gate has passed.
			case proto.PartitionProbeSkipped:
				capability.Reason = reason(unavailableRuntime, probe.Detail)
				out = append(out, capability)
				continue
			case proto.PartitionProbeFailed:
				capability.Reason = reason(unavailableProbeFailed, probe.Detail)
				out = append(out, capability)
				continue
			default:
				capability.Reason = reason(unavailableProbePending, probe.Detail)
				out = append(out, capability)
				continue
			}
		}
		capability.Status = StatusReady
		out = append(out, capability)
	}
	return out
}

func compatibilityNotes(backend accelerator.Backend, node *store.Node, gpu *store.GPU) []string {
	profile, profileWarning := accelerator.ProfileCompatibility(backend.ID(), node.Framework, node.FrameworkVersion, node.Arch)
	var notes []string
	if profileWarning != "" {
		notes = append(notes, "host runtime combination is outside the built matrix: "+profileWarning)
	} else if profile.Backend != "" && !profile.Verified {
		notes = append(notes, "this runtime combination is not yet verified on provisioned real hardware; this node follows the dGPU probe")
	}
	if len(profile.ChipFamilies) > 0 && strings.TrimSpace(gpu.ArchID) != "" {
		matched := false
		for _, family := range profile.ChipFamilies {
			matched = matched || strings.Contains(strings.ToLower(gpu.ArchID), strings.ToLower(family))
		}
		if !matched {
			notes = append(notes, "chip family "+gpu.ArchID+" is not in this asset's verified list")
		}
	}
	notes = append(notes, accelerator.HostRequirementNotes(backend)...)
	return notes
}

func ForNode(node *store.Node) []proto.PartitionCapability {
	if node == nil {
		return nil
	}
	byKey := map[string]proto.PartitionCapability{}
	reports := Decode(node)
	owned := node.OwnedGPUs()
	for i := range owned {
		for _, capability := range reports.ForGPU(&owned[i]) {
			key := strings.Join([]string{capability.Backend, capability.Status, capability.Reason}, "\x00")
			byKey[key] = capability
		}
	}
	out := make([]proto.PartitionCapability, 0, len(byKey))
	for _, capability := range byKey {
		out = append(out, capability)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Backend < out[j].Backend })
	return out
}

// Preferred is the capability a memory quota that names no backend uses on
// this node: a ready one, the node's own accelerator vendor before NVIDIA's
// (the order the deploy form shows them in). ok is false when no backend on the
// node is ready.
func Preferred(node *store.Node) (proto.PartitionCapability, bool) {
	if node == nil {
		return proto.PartitionCapability{}, false
	}
	if _, present := assetReports(node.PartitionAssets); !present {
		// Every backend needs its assets reported before it can be ready, so
		// a node with none answers without deriving a capability per card:
		// an automatic evaluation asks this of every eligible node.
		return proto.PartitionCapability{}, false
	}
	var fallback *proto.PartitionCapability
	for _, capability := range ForNode(node) {
		if capability.Status != StatusReady {
			continue
		}
		if capability.Vendor != accelerator.VendorNVIDIA {
			return capability, true
		}
		if fallback == nil {
			found := capability
			fallback = &found
		}
	}
	if fallback == nil {
		return proto.PartitionCapability{}, false
	}
	return *fallback, true
}

func ReadyBackendIDs(node *store.Node) []string {
	seen := map[string]bool{}
	for _, capability := range ForNode(node) {
		if capability.Status == StatusReady {
			seen[capability.Backend] = true
		}
	}
	out := make([]string, 0, len(seen))
	for backend := range seen {
		out = append(out, backend)
	}
	sort.Strings(out)
	return out
}

func assetReports(raw string) (map[string][]proto.AssetReport, bool) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return nil, false
	}
	var reports []proto.AssetReport
	if err := json.Unmarshal([]byte(raw), &reports); err != nil {
		return nil, false
	}
	byBackend := make(map[string][]proto.AssetReport, len(reports))
	for _, report := range reports {
		if _, ok := accelerator.Lookup(report.Backend); !ok {
			continue
		}
		byBackend[report.Backend] = append(byBackend[report.Backend], report)
	}
	return byBackend, true
}

type probeKey struct {
	backend  string
	gpuIndex int
}

func probeReports(raw string) (map[probeKey]proto.PartitionProbeResult, map[string]bool, bool) {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return nil, nil, false
	}
	var reports []proto.PartitionProbeResult
	if err := json.Unmarshal([]byte(raw), &reports); err != nil {
		return nil, nil, false
	}
	byGPU := make(map[probeKey]proto.PartitionProbeResult, len(reports))
	legacy := map[string]bool{}
	for _, report := range reports {
		if _, ok := accelerator.Lookup(report.Backend); !ok {
			continue
		}
		if report.GPUIndex == nil {
			legacy[report.Backend] = true
			continue
		}
		key := probeKey{backend: report.Backend, gpuIndex: *report.GPUIndex}
		previous, exists := byGPU[key]
		if !exists || report.CheckedAt.After(previous.CheckedAt) {
			byGPU[key] = report
		}
	}
	return byGPU, legacy, true
}

func missingAssets(required []string, reports []proto.AssetReport) []string {
	// Required files must coexist under one reported root. Unioning files from
	// separate directories can claim readiness even though the execution path
	// cannot select any complete asset bundle.
	for _, report := range reports {
		available := map[string]bool{}
		for _, file := range report.Files {
			available[strings.TrimSpace(file)] = true
		}
		complete := true
		for _, file := range required {
			if !available[file] {
				complete = false
				break
			}
		}
		if complete {
			return nil
		}
	}
	available := map[string]bool{}
	if len(reports) > 0 {
		// For diagnostics, use the most complete single report rather than a
		// cross-root union so the reason names files execution still lacks.
		bestCount := -1
		for _, report := range reports {
			candidate := map[string]bool{}
			for _, file := range report.Files {
				candidate[strings.TrimSpace(file)] = true
			}
			count := 0
			for _, file := range required {
				if candidate[file] {
					count++
				}
			}
			if count > bestCount {
				bestCount, available = count, candidate
			}
		}
	}
	var missing []string
	for _, file := range required {
		if !available[file] {
			missing = append(missing, file)
		}
	}
	sort.Strings(missing)
	return missing
}

func ValidateReady(node *store.Node, backendID string) error {
	backend, ok := accelerator.Lookup(backendID)
	if !ok {
		return fmt.Errorf("unknown partition backend %q", backendID)
	}
	for _, capability := range ForNode(node) {
		if capability.Backend == backend.ID() {
			if capability.Status == StatusReady {
				return nil
			}
			return fmt.Errorf("partition backend %s is unavailable: %s", backend.ID(), capability.Reason)
		}
	}
	return fmt.Errorf("partition backend %s does not match the selected node", backend.ID())
}
