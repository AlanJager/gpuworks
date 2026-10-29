// Package simulate is the agent's simulation mode, for the experience
// environment: the node is a machine described by a file rather than the
// host the agent runs on. Its cards are simulated (with metrics that follow
// what runs on them), its runtime starts no containers (each "container"
// asks the experience environment to answer for its model on its port), and
// the agent refuses whatever would reach the real host (hostsim).
// A real node never loads this package's types.
package simulate

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// EnvSpec names the file describing the simulated machine.
const EnvSpec = "GPUWORKS_SIMULATED_NODE"

// Spec is the machine a simulated node stands for, and where the experience
// environment that serves its engines listens.
type Spec struct {
	Hostname string `json:"hostname"`
	Address  string `json:"address"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kernel   string `json:"kernel"`
	CPU      string `json:"cpu"`
	Cores    int    `json:"cores"`
	MemGiB   int64  `json:"mem_gib"`

	Vendor           string `json:"vendor"`
	Framework        string `json:"framework"`
	FrameworkVersion string `json:"framework_version"`
	Driver           string `json:"driver"`
	Card             string `json:"card"`
	Count            int    `json:"count"`
	VRAMMiB          int64  `json:"vram_mib"`
	// ComputeCapability is an NVIDIA card's; ArchID another vendor's
	// architecture (PPU: Beethoven).
	ComputeCapability string `json:"compute_capability,omitempty"`
	ArchID            string `json:"arch_id,omitempty"`
	// TDP is a busy card's power draw in watts.
	TDP float64 `json:"tdp"`

	// Engines is the experience environment's engine host: its base URL and
	// the token a node presents to it.
	Engines      string `json:"engines"`
	EnginesToken string `json:"engines_token"`
	// PortSlot is the node's share of the engine port range, of PortSlots
	// equal ones: the simulated nodes share a host (Prober.EnginePortListeners).
	PortSlot  int `json:"port_slot,omitempty"`
	PortSlots int `json:"port_slots,omitempty"`
	// ParentPID is the experience environment's process: the node stops
	// once it is gone, so none outlives the environment.
	ParentPID int `json:"parent_pid,omitempty"`
}

// Load reads and checks a spec file.
func Load(path string) (Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	var s Spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return Spec{}, fmt.Errorf("simulated node %s: %w", path, err)
	}
	switch {
	case strings.TrimSpace(s.Hostname) == "":
		return Spec{}, fmt.Errorf("simulated node %s: hostname is required", path)
	case s.Count < 0 || s.Count > 64:
		return Spec{}, fmt.Errorf("simulated node %s: card count %d is out of range", path, s.Count)
	case s.Count > 0 && (s.Card == "" || s.VRAMMiB <= 0 || s.Vendor == ""):
		return Spec{}, fmt.Errorf("simulated node %s: cards need a vendor, a model and their memory", path)
	case s.PortSlots < 0 || s.PortSlot < 0 || (s.PortSlots > 0 && s.PortSlot >= s.PortSlots):
		return Spec{}, fmt.Errorf("simulated node %s: port slot %d of %d is out of range", path, s.PortSlot, s.PortSlots)
	case s.Engines == "":
		return Spec{}, fmt.Errorf("simulated node %s: the engine host is required", path)
	}
	return s, nil
}
