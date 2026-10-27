// pd.go holds the launch facts a PD disaggregated instance adds to an
// ordinary launch (ADR 0028): which role it plays, where its KV side channel
// listens, and which network device the KV transfer uses. The rules behind
// them (connector class, the connector's role names) come from package pdmode.

package deploy

import (
	"strconv"

	"gpuworks.ai/gpuworks/internal/server/pdmode"
)

// PDLaunch is one PD instance's role-specific launch input.
type PDLaunch struct {
	Mode string
	Role string
	// KVLoadFailure is the engine policy when a pull fails (pdmode.KVLoad*).
	KVLoadFailure string
	// SideChannelHost is the address the KV side channel listens on: the
	// node's address on the network the PD group shares. The engine default
	// is localhost, which no other node can reach.
	SideChannelHost string
	// SideChannelPort is the first port of the instance's extra port block
	// (NIXL side channels, a bootstrap server, a Mooncake kv_port band).
	SideChannelPort int
	// RoleTP is every role's tensor parallel degree, for connectors whose
	// config names both sides.
	RoleTP map[string]int
}

// mode returns the declared mode; an unknown name yields the zero Mode, which
// injects nothing (placement validation rejects it long before a launch).
func (p *PDLaunch) mode() pdmode.Mode {
	if p == nil {
		return pdmode.Mode{}
	}
	mode, _ := pdmode.Lookup(p.Mode)
	return mode
}

// PDRoleLaunchConfig is the launch configuration a role runs with. KV offload
// belongs to the prefill role only: on the decoder the PD connector always
// supplies the prefix first, so a cache tier there only writes (measured: 14
// GB written, 0 bytes read back) and was reported to crash on first hit. The
// deployment keeps one configuration; this is where the decode role drops the
// tiers, for the argv and the disk mount alike.
func PDRoleLaunchConfig(q LaunchConfig, role string) LaunchConfig {
	if role != pdmode.RoleDecode {
		return q
	}
	q.KVOffloadingSize = 0
	q.KVDiskOffload = false
	q.SharedKVDir = ""
	q.KVDiskLimitGiB = 0
	q.KVOffloadReadThreads, q.KVOffloadWriteThreads, q.KVOffloadBlockSize = 0, 0, 0
	return q
}

// kvTransferNetworkEnv selects the network device the KV transfer (UCX under
// NIXL) uses on a PD instance. RDMA networks get one HCA and are held to RDMA
// and CUDA transports, so a container that cannot use verbs fails at startup
// instead of crawling over TCP. Ethernet networks name the interface: with
// host networking UCX sees every host device and would otherwise pick bridges
// such as docker0. UCX filters bridge devices by default, and a named bridge
// then leaves NIXL without a backend (NIXL_ERR_BACKEND, measured on a host
// whose only address is on a bridge), so naming a device also lifts that
// filter.
func kvTransferNetworkEnv(p Params) map[string]string {
	if p.PD == nil || p.PD.mode().Transport != pdmode.TransportUCX {
		return nil
	}
	n := p.ComputeNetwork
	env := map[string]string{}
	switch {
	case n.HostRDMA() && len(n.HCAs) > 0:
		env["UCX_NET_DEVICES"] = n.HCAs[0]
		env["UCX_TLS"] = "rc,cuda_copy,cuda_ipc,gdr_copy"
	case n.Interface != "":
		env["UCX_NET_DEVICES"] = n.Interface
		env["UCX_TCP_BRIDGE_ENABLE"] = "y"
	}
	return env
}

// vllmPDEnv is the vLLM side channel environment of a PD instance, as the
// mode declares it.
func vllmPDEnv(p Params, env map[string]string) {
	if p.PD == nil {
		return
	}
	mode := p.PD.mode()
	for key, value := range mode.Env {
		env[key] = value
	}
	if mode.SideChannel == pdmode.SideChannelNIXL {
		if p.PD.SideChannelHost != "" {
			env["VLLM_NIXL_SIDE_CHANNEL_HOST"] = p.PD.SideChannelHost
		}
		if p.PD.SideChannelPort > 0 {
			env["VLLM_NIXL_SIDE_CHANNEL_PORT"] = strconv.Itoa(p.PD.SideChannelPort)
		}
	}
	if p.PD.SideChannelHost != "" {
		for _, key := range mode.AddressEnv {
			env[key] = p.PD.SideChannelHost
		}
	}
	if iface := p.ComputeNetwork.Interface; iface != "" {
		for _, key := range mode.InterfaceEnv {
			env[key] = iface
		}
	}
	for key, value := range kvTransferNetworkEnv(p) {
		env[key] = value
	}
}

// PDModeProfile is one PD mode as the deploy form sees it: what it offers
// and the rules it checks, all read from package pdmode.
type PDModeProfile struct {
	Name string `json:"name"`
	// Vendors are the accelerator vendors the mode runs on.
	Vendors []string `json:"vendors"`
	// Roles in the mode's order; ScalableToZero are those a running
	// deployment may shrink to no instances.
	Roles          []string `json:"roles"`
	ScalableToZero []string `json:"scalable_to_zero"`
	// TPRule constrains the two roles' TP (pdmode.TPRule values).
	TPRule          string `json:"tp_rule"`
	MaxUnitsPerRole int    `json:"max_units_per_role"`
	// Fallbacks and KVLoadFailures are the allowed policy values, the
	// default first; KVLoadFailures is empty when the engine has no such
	// setting.
	Fallbacks      []string `json:"fallbacks"`
	KVLoadFailures []string `json:"kv_load_failures"`
	Unverified     bool     `json:"unverified,omitempty"`
}

func pdModeProfiles(engine string) []PDModeProfile {
	var out []PDModeProfile
	for _, name := range pdmode.ForEngine(engine) {
		mode, _ := pdmode.Lookup(name)
		out = append(out, PDModeProfile{
			Name: mode.Name, Vendors: nonNil(mode.Vendors), Roles: nonNil(mode.Roles), ScalableToZero: nonNil(mode.ScalableToZero),
			TPRule: string(mode.TP), MaxUnitsPerRole: pdmode.MaxUnitsPerRole,
			Fallbacks: nonNil(mode.Fallbacks()), KVLoadFailures: nonNil(mode.KVLoadFailures), Unverified: mode.Unverified,
		})
	}
	return out
}

// nonNil keeps an empty list a JSON array.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return append([]string(nil), list...)
}
