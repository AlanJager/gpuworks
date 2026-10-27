// Package pdmode is the catalog of PD (prefill/decode) disaggregation modes:
// the one place that says which roles a mode has, how its KV connector names
// them, how the gateway hands a request from one role to the other, and which
// pairing rules the two roles must satisfy (ADR 0028). Placement validation,
// launch assembly, the controller and the gateway all read these declarations
// instead of writing the rules down again.
//
// It is a leaf package: it imports only other leaves (engine and vendor
// names, product error codes) and does no I/O.
package pdmode

import (
	"fmt"
	"sort"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Role names. A role is a string on the instance row, so a later mode may add
// an auxiliary role without a schema change; these are the two every mode so
// far has.
const (
	RolePrefill = "prefill"
	RoleDecode  = "decode"
)

// MaxUnitsPerRole bounds the instances of one role, matching the replica
// ceiling of an ordinary deployment.
const MaxUnitsPerRole = 32

// Fallback is what the gateway does when no prefill instance can take a
// request. Which ones a mode allows, and its default, is Mode.Fallbacks.
const (
	// FallbackDecode sends the request to a decode instance, which then
	// computes the prefill itself. It is the default where the decoder can.
	FallbackDecode = "decode"
	// FallbackFail answers with a retryable 503 instead.
	FallbackFail = "fail"
)

// KV load failure policies: what a decode engine does when pulling the KV
// from the prefill instance fails after the handoff. Which ones a mode's
// engine takes, and its default, is Mode.KVLoadFailures.
const (
	KVLoadFail      = "fail"
	KVLoadRecompute = "recompute"
)

// Wire is how the gateway hands a request from prefill to decode.
type Wire string

const (
	// WireSequential sends the request to the prefill instance first, with
	// one output token and a marker asking it to keep the KV for a remote
	// decoder, then forwards the kv_transfer_params it returns to the decode
	// instance, which pulls the KV itself.
	WireSequential Wire = "sequential"
	// WireConcurrent sends the same request to both instances at once, both
	// carrying a bootstrap address (the prefill instance's bootstrap server)
	// and a room number that pairs them; the decode instance's answer is the
	// client's (SGLang).
	WireConcurrent Wire = "concurrent"
)

// TPRule constrains the tensor parallel degrees of the two roles.
type TPRule string

const (
	// TPDecodeAtLeastPrefill requires decode TP >= prefill TP (NIXL).
	TPDecodeAtLeastPrefill TPRule = "decode_at_least_prefill"
	// TPPrefillAtLeastDecode requires prefill TP >= decode TP (vllm-ascend's
	// Mooncake connector gathers one decode rank's KV from several prefill
	// ranks, the reverse of NIXL).
	TPPrefillAtLeastDecode TPRule = "prefill_at_least_decode"
	// TPEqual requires both roles to use the same TP.
	TPEqual TPRule = "equal"
)

// SideChannel is how an instance tells its peers where it is.
type SideChannel string

const (
	// SideChannelNIXL sets vLLM's NIXL side channel host and port.
	SideChannelNIXL SideChannel = "nixl"
	// SideChannelHostIP makes the connector read the node address from the
	// mode's AddressEnv (vLLM's get_ip honours VLLM_HOST_IP).
	SideChannelHostIP SideChannel = "host_ip"
)

// Transport is what carries the KV between the roles.
type Transport string

const (
	// TransportUCX is UCX under NIXL: the host network, so the UCX device
	// rules apply (network device, RDMA transports, bridge switch).
	TransportUCX Transport = "ucx"
	// TransportHCCL is the NPUs' own RoCE ports: across hosts it needs an
	// accelerator RDMA network, and the host network only for the handshake.
	TransportHCCL Transport = "hccl"
)

// Mode is one PD recipe.
type Mode struct {
	Name   string
	Engine string
	// Vendors are the accelerator vendors the mode is declared for.
	Vendors []string
	// SameVendor is a pairing rule between the two roles.
	SameVendor bool
	// Roles lists the roles in the order the gateway picks them: decode first
	// so a later conditional disaggregation can look at the decoder's cache.
	Roles []string
	// Connector is the engine's KV connector class and ConnectorRoles maps a
	// role to the connector's own role value.
	Connector      string
	ConnectorRoles map[string]string
	Wire           Wire
	TP             TPRule
	// TwoStagePaths are the request paths the gateway disaggregates; every
	// other path goes to a decode instance directly.
	TwoStagePaths []string
	// TransferMetricsRole is the role whose KV transfer counters are
	// meaningful: NIXL is pulled by the decoder, so the prefill side stays 0.
	TransferMetricsRole string
	// TransferBytesMetric is the Prometheus family on that role that counts
	// transferred bytes (a histogram's _sum); empty when the engine exposes
	// none, and a handoff is then confirmed by its answer alone.
	TransferBytesMetric string
	// VerifiedVersions is the engine version range the mode was verified on.
	// Launching outside it is allowed and only warned about.
	VerifiedVersions string
	// ScalableToZero lists the roles a running deployment may shrink to no
	// instances: without prefill instances every request is decoded alone,
	// which the decoder can do (plan D14). A deployment is still created
	// with every role.
	ScalableToZero []string
	// DecodeServesAlone reports whether a decode instance can answer a
	// request with no prefill instance involved (it then computes the
	// prefill itself). Without it there is no fallback: a request no prefill
	// instance can take is answered with a retryable 503.
	DecodeServesAlone bool
	// KVLoadFailures are the KV load failure policies the engine takes, the
	// default first; none when the engine has no such setting.
	KVLoadFailures []string
	// ExtraPortsPerTP and ExtraPorts size each role's block of ports beyond
	// the API port: per TP rank (NIXL side channels) or fixed (a bootstrap
	// server).
	ExtraPortsPerTP map[string]int
	ExtraPortsFixed map[string]int
	// Unverified marks a mode no release has been verified on real hardware;
	// evaluation warns about it.
	Unverified bool

	// SideChannel is how an instance tells peers where it is.
	SideChannel SideChannel
	// AddressEnv are variables set to the instance's node address and
	// InterfaceEnv to the network interface holding it (HCCL handshakes).
	AddressEnv   []string
	InterfaceEnv []string
	// Env is fixed environment every instance of the mode needs; a template
	// or the deployment may still set another value.
	Env map[string]string
	// Transport is what carries the KV.
	Transport Transport
	// ConnectorPortKey names the connector config field that takes the base
	// of the instance's extra port band (vllm-ascend's kv_port), and
	// ConnectorParallelism puts both roles' tp/dp into the connector config.
	ConnectorPortKey     string
	ConnectorParallelism bool
}

// ascendCards is what HCCL needs to be told on an Ascend host; the mode that
// hands KV to HCCL sets the same variables an ordinary multi-node rank gets.
var ascendCards, _ = accelerator.CardNetworkOf(accelerator.VendorAscend)

var modes = map[string]Mode{
	"vllm-nixl": {
		Name:                "vllm-nixl",
		Engine:              engines.VLLM,
		Vendors:             []string{accelerator.VendorNVIDIA},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "NixlConnector",
		ConnectorRoles:      map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                WireSequential,
		TP:                  TPDecodeAtLeastPrefill,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole: RoleDecode,
		TransferBytesMetric: "vllm:nixl_bytes_transferred",
		VerifiedVersions:    "0.17.1-0.29.1",
		ScalableToZero:      []string{RolePrefill},
		DecodeServesAlone:   true,
		KVLoadFailures:      []string{KVLoadFail, KVLoadRecompute},
		ExtraPortsPerTP:     map[string]int{RolePrefill: 1, RoleDecode: 1},
		SideChannel:         SideChannelNIXL,
		// NixlConnector transfers a Mamba-style layer's conv state (gated
		// delta net, KDA, short conv) only in the dim-first layout and refuses
		// to start otherwise; vLLM's default is state-first. The variable means
		// nothing to a model without such layers, which is why vLLM's own CPU
		// platform sets it whenever a NIXL connector is configured.
		Env:       map[string]string{"VLLM_SSM_CONV_STATE_LAYOUT": "DS"},
		Transport: TransportUCX,
	},
	// vllm-ascend with its MooncakeConnectorV1: the same kv_transfer_params
	// wire as NIXL, KV over the NPUs' RoCE ports (HCCL), handshakes on the
	// node's control network. MooncakeConnectorV1 rather than
	// MooncakeHybridConnector: when a transfer fails V1 answers 500, while
	// the hybrid connector was measured to answer 200 with garbage.
	// Verified across two 910B4 hosts (vllm-ascend 0.26.0rc1, driver
	// 26.0.rc1): the prefill and decode NPUs must reach each other on the NPU
	// network; the lab's hosts are cabled NPU to NPU, card N to card N, so
	// only same-index pairs across the two hosts can. The earlier failure
	// (HcclCommPrepare) was a same-host pair, whose ports are not linked.
	"vllm-ascend-mooncake": {
		Name:                 "vllm-ascend-mooncake",
		Engine:               engines.VLLM,
		Vendors:              []string{accelerator.VendorAscend},
		SameVendor:           true,
		Roles:                []string{RoleDecode, RolePrefill},
		Connector:            "MooncakeConnectorV1",
		ConnectorRoles:       map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                 WireSequential,
		TP:                   TPPrefillAtLeastDecode,
		TwoStagePaths:        []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole:  RoleDecode,
		ScalableToZero:       []string{RolePrefill},
		DecodeServesAlone:    true,
		KVLoadFailures:       []string{KVLoadFail, KVLoadRecompute},
		ExtraPortsPerTP:      map[string]int{RolePrefill: 1, RoleDecode: 1},
		SideChannel:          SideChannelHostIP,
		AddressEnv:           append([]string{"VLLM_HOST_IP"}, ascendCards.AddressEnv...),
		InterfaceEnv:         append(append([]string(nil), ascendCards.InterfaceEnv...), "GLOO_SOCKET_IFNAME", "TP_SOCKET_IFNAME"),
		Transport:            TransportHCCL,
		ConnectorPortKey:     "kv_port",
		ConnectorParallelism: true,
	},
	// SGLang with the NIXL transfer backend: UCX carries the KV, so the
	// network rules are vllm-nixl's. The decoder cannot compute a prefill in
	// disaggregation mode, so there is no fallback and no scaling prefill to
	// zero. Both roles keep one TP until heterogeneous TP is verified.
	"sglang-nixl": {
		Name:                "sglang-nixl",
		Engine:              engines.SGLang,
		Vendors:             []string{accelerator.VendorNVIDIA},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "nixl",
		ConnectorRoles:      map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                WireConcurrent,
		TP:                  TPEqual,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		Unverified:          true,
		Transport:           TransportUCX,
	},
}

// Lookup returns the mode with this name.
func Lookup(name string) (Mode, bool) {
	mode, ok := modes[name]
	return mode, ok
}

// Names lists every mode, sorted.
func Names() []string {
	out := make([]string, 0, len(modes))
	for name := range modes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ForEngine lists the modes an engine supports, sorted.
func ForEngine(engine string) []string {
	var out []string
	for _, name := range Names() {
		if modes[name].Engine == engine {
			out = append(out, name)
		}
	}
	return out
}

// HasRole reports whether the mode declares this role.
func (m Mode) HasRole(role string) bool {
	return contains(m.Roles, role)
}

// MayBeEmpty reports whether a running deployment may have no instance of
// this role.
func (m Mode) MayBeEmpty(role string) bool {
	return contains(m.ScalableToZero, role)
}

// SupportsVendor reports whether the mode is declared for this vendor.
func (m Mode) SupportsVendor(vendor string) bool {
	return contains(m.Vendors, vendor)
}

// CheckTP validates the two roles' tensor parallel degrees against the mode's
// direction rule; the error is a product error naming the rule.
func (m Mode) CheckTP(prefill, decode int) error {
	params := map[string]any{"mode": m.Name, "prefill": prefill, "decode": decode}
	switch m.TP {
	case TPDecodeAtLeastPrefill:
		if decode < prefill {
			return messages.NewProductError(messages.ErrorPDTPDirection, fmt.Sprintf("%s needs decode TP >= prefill TP, got prefill %d and decode %d", m.Name, prefill, decode), params)
		}
	case TPPrefillAtLeastDecode:
		if prefill < decode {
			return messages.NewProductError(messages.ErrorPDTPPrefillAtLeastDecode, fmt.Sprintf("%s needs prefill TP >= decode TP, got prefill %d and decode %d", m.Name, prefill, decode), params)
		}
	case TPEqual:
		if decode != prefill {
			return messages.NewProductError(messages.ErrorPDTPEqual, fmt.Sprintf("%s needs the same TP on both roles, got prefill %d and decode %d", m.Name, prefill, decode), params)
		}
	}
	return nil
}

// ExtraPorts is how many consecutive ports beyond the API port one instance
// of a role with this TP listens on: NIXL offsets a worker's side channel by
// its rank, so a TP group needs TP of them; a bootstrap server needs one.
func (m Mode) ExtraPorts(role string, tp int) int {
	if tp < 1 {
		tp = 1
	}
	return m.ExtraPortsPerTP[role]*tp + m.ExtraPortsFixed[role]
}

// TwoStage reports whether a request path is disaggregated.
func (m Mode) TwoStage(path string) bool {
	return contains(m.TwoStagePaths, path)
}

// ConnectorRole is the connector's role value for a PD role.
func (m Mode) ConnectorRole(role string) string {
	return m.ConnectorRoles[role]
}

// Fallbacks lists the gateway fallbacks the mode allows, its default first:
// a mode whose decoder serves alone may fall back to it, any mode may fail.
func (m Mode) Fallbacks() []string {
	if m.DecodeServesAlone {
		return []string{FallbackDecode, FallbackFail}
	}
	return []string{FallbackFail}
}

// EffectiveFallback is the fallback a placement's value means: empty is the
// mode's default.
func (m Mode) EffectiveFallback(value string) string {
	if value == "" {
		return m.Fallbacks()[0]
	}
	return value
}

// ValidFallback reports whether the mode allows a fallback value; empty
// means the default.
func (m Mode) ValidFallback(value string) bool {
	return value == "" || contains(m.Fallbacks(), value)
}

// EffectiveKVLoadFailure is the policy a placement's value means: empty is
// the mode's default, and a mode whose engine has no such setting has none.
func (m Mode) EffectiveKVLoadFailure(value string) string {
	if value == "" && len(m.KVLoadFailures) > 0 {
		return m.KVLoadFailures[0]
	}
	return value
}

// ValidKVLoadFailure reports whether the mode's engine takes a policy value;
// empty means the default.
func (m Mode) ValidKVLoadFailure(value string) bool {
	return value == "" || contains(m.KVLoadFailures, value)
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
