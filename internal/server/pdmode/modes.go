package pdmode

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
)

// ascendCards is what HCCL needs to be told on an Ascend host; the mode that
// hands KV to HCCL sets the same variables an ordinary multi-node rank gets.
var ascendCards, _ = accelerator.CardNetworkOf(accelerator.VendorAscend)

var modes = map[string]Mode{
	"vllm-nixl": {
		Name:   "vllm-nixl",
		Engine: engines.VLLM,
		// AMD: nixl-rocm over UCX TCP, validated across two MI300X hosts.
		Vendors:             []string{accelerator.VendorNVIDIA, accelerator.VendorAMD},
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
	// zero. The TP pairing is SGLang's (see TPAny); a pairing it cannot
	// make fails the decode instance's start, not the platform's checks.
	"sglang-nixl": {
		Name:                "sglang-nixl",
		Engine:              engines.SGLang,
		Vendors:             []string{accelerator.VendorNVIDIA},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "nixl",
		ConnectorRoles:      map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                WireConcurrent,
		TP:                  TPAny,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		Unverified:          true,
		Transport:           TransportUCX,
	},
	// SGLang on T-Head PPU with Mooncake's RDMA transport: the KV goes card
	// to NIC to card, across hosts on a host RDMA network or between two cards
	// of one host. The NIC registers the cards' memory through the PPU
	// driver's peer-memory module (alixpu_peermem), so every node needs it
	// loaded. None of the IPC mode's patches is involved. Verified on PPU
	// ZW810E (RoCE, mlx5) with TP 1 on both roles; other pairings are
	// SGLang's to accept (TPAny).
	"sglang-ppu-mooncake": {
		Name:                "sglang-ppu-mooncake",
		Engine:              engines.SGLang,
		Vendors:             []string{accelerator.VendorPPU},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "mooncake",
		ConnectorRoles:      map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                WireConcurrent,
		TP:                  TPAny,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		VerifiedVersions:    "0.5.13",
		// First requests of a shape compile PPU kernels for minutes; a
		// generating health check would time out and retire the instance.
		Env:                  map[string]string{"SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION": "0"},
		FirstRequestCompiles: true,
		PeerMemory:           true,
		Transport:            TransportMooncakeRDMA,
	},
	// vLLM's MooncakeConnector on T-Head PPU over RDMA, as sglang-ppu-mooncake.
	// The prefill engine answers its one-token copy without kv_transfer_params;
	// the decoder pulls by the prefill engine's id from its bootstrap server
	// (VLLM_MOONCAKE_BOOTSTRAP_PORT), so the gateway composes the handoff.
	// Verified on PPU ZW810E with vLLM 0.23.0 and TP 1 on both roles; other
	// pairings are the engine's (TPAny).
	"vllm-ppu-mooncake": {
		Name:                "vllm-ppu-mooncake",
		Engine:              engines.VLLM,
		Vendors:             []string{accelerator.VendorPPU},
		SameVendor:          true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "MooncakeConnector",
		ConnectorRoles:      map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                WireSequential,
		TP:                  TPAny,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		VerifiedVersions:    "0.23.0",
		ConnectorExtra:      map[string]any{"mooncake_protocol": "rdma"},
		SynthesizedHandoff:  true,
		PeerMemory:          true,
		Transport:           TransportMooncakeRDMA,
	},
	// vLLM's MooncakeConnector on Hygon DCU: the Hygon vLLM 0.21 image runs it
	// on a Mooncake transfer engine built for DTK (TCP, RDMA and an intra-host
	// HIP transport), patched by vllm-hcu, so the KV moves over whatever host
	// network the group shares. A decoder given no handoff computes the
	// prefill itself. The vllm-hcu patch is about heterogeneous TP/PP; only
	// equal TP was tried. Verified on K100, two cards in one host at TP 1. It
	// needs the image's r2 or later: r1's connector reported a failed pull as
	// finished, so the decoder answered from KV it never received and the
	// load failure policy did not apply. Hybrid models with gated delta net
	// layers (Qwen3.5/3.6/3.8) need r4, whose connector moves their KV pages
	// whole at equal TP and whose scheduler survives their failed pulls and
	// does not zero blocks a pull is writing (r3 can), and the hybrid KV cache
	// manager (verified on K100 with Qwen3.5-0.8B and
	// hygon/Qwen3.8-27B-Channel-INT8-w8a8). The TP pairing is the engine's
	// (TPAny): unequal TP is untried, hybrid models' whole-page move above
	// is r4's at equal TP only.
	"vllm-mooncake": {
		Name:                 "vllm-mooncake",
		Engine:               engines.VLLM,
		Vendors:              []string{accelerator.VendorHygon},
		SameVendor:           true,
		Roles:                []string{RoleDecode, RolePrefill},
		Connector:            "MooncakeConnector",
		ConnectorRoles:       map[string]string{RolePrefill: "kv_producer", RoleDecode: "kv_consumer"},
		Wire:                 WireSequential,
		TP:                   TPAny,
		TwoStagePaths:        []string{"/v1/chat/completions", "/v1/completions"},
		TransferMetricsRole:  RoleDecode,
		VerifiedVersions:     "0.21.0",
		ScalableToZero:       []string{RolePrefill},
		DecodeServesAlone:    true,
		KVLoadFailures:       []string{KVLoadFail, KVLoadRecompute},
		ExtraPortsFixed:      map[string]int{RolePrefill: 1},
		SynthesizedHandoff:   true,
		Transport:            TransportMooncake,
		HybridKVCacheManager: true,
	},
	// SGLang on Hygon DCU with Mooncake's RDMA transport between the hosts'
	// RDMA NICs (Hygon's SGLang builds for DTK carry the Mooncake transfer
	// engine). The roles pair by SGLang's own rules: by attention TP, which
	// unequal widths in either direction satisfy, so the platform only checks
	// the pipeline pairing it can see. A multi-node prefill instance serves
	// its bootstrap on one port on every rank, as hand-written Hygon
	// launches do. Not yet run end to end on the platform.
	"sglang-mooncake": {
		Name:                       "sglang-mooncake",
		Engine:                     engines.SGLang,
		Vendors:                    []string{accelerator.VendorHygon},
		SameVendor:                 true,
		Roles:                      []string{RoleDecode, RolePrefill},
		Connector:                  "mooncake",
		ConnectorRoles:             map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                       WireConcurrent,
		TP:                         TPAny,
		DecodePipelineMatchesOrOne: true,
		SharedSideChannel:          true,
		TwoStagePaths:              []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole:        RoleDecode,
		ExtraPortsFixed:            map[string]int{RolePrefill: 1},
		Unverified:                 true,
		// A generating health check queues a fake request on the prefill
		// side, and a large model's first requests are slow to answer.
		Env:       map[string]string{"SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION": "0"},
		Transport: TransportMooncakeRDMA,
	},
	// SGLang on T-Head PPU with the mooncake backend: Mooncake's NVLink
	// transport over legacy IPC handles copies the KV card to card inside one
	// host, so the two roles share a node. The PPU SGLang runner carries the
	// patches the variables switch on (build/runner-images/hggc/ppu-pd):
	// registering the KV by allocation (an IPC handle names a whole
	// allocation), metadata on the card, and serialized transfers - an IPC
	// copy issued while a forward pass runs can deadlock the PPU driver, which
	// is also why the overlap scheduler is off. The health endpoint answers
	// without a generation: the first requests of a new shape compile PPU
	// kernels for minutes, a generating health check times out meanwhile and
	// the agent would retire a healthy instance, and every such check would
	// queue a fake transfer on the prefill side. Verified on PPU ZW810E with
	// TP 1 on both roles; other pairings are the engine's (TPAny).
	"sglang-ppu-ipc": {
		Name:                "sglang-ppu-ipc",
		Engine:              engines.SGLang,
		Vendors:             []string{accelerator.VendorPPU},
		SameVendor:          true,
		SameNode:            true,
		Roles:               []string{RoleDecode, RolePrefill},
		Connector:           "mooncake",
		ConnectorRoles:      map[string]string{RolePrefill: "prefill", RoleDecode: "decode"},
		Wire:                WireConcurrent,
		TP:                  TPAny,
		TwoStagePaths:       []string{"/v1/chat/completions", "/v1/completions", "/generate"},
		TransferMetricsRole: RoleDecode,
		ExtraPortsFixed:     map[string]int{RolePrefill: 1},
		VerifiedVersions:    "0.5.13",
		Env: map[string]string{
			"SGLANG_PPU_PD_IPC":                        "1",
			"SGLANG_PPU_PD_SERIAL_TRANSFER":            "1",
			"MC_FORCE_MNNVL":                           "1",
			"MC_USE_NVLINK_IPC":                        "1",
			"SGLANG_ENABLE_HEALTH_ENDPOINT_GENERATION": "0",
		},
		EngineArgs:           []string{"--disable-overlap-schedule"},
		FirstRequestCompiles: true,
		Transport:            TransportIPC,
	},
}
