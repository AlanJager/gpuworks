package dto

import (
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

// NodeTopology is a node's GPU interconnect as the agent's topology probe
// reported it (topoparse.Topology).
type NodeTopology struct {
	GPUs   []TopologyGPU    `json:"gpus"`
	Matrix [][]TopologyLink `json:"matrix"` // matrix[i][j] is the link between GPU i and j
	// FastCliques are the maximal groups of GPUs every pair of which shares a
	// fast link; NVLinkCliques the same over NVLink only.
	FastCliques   [][]int       `json:"fast_cliques"`
	NVLinkCliques [][]int       `json:"nvlink_cliques"`
	NUMAGroups    map[int][]int `json:"numa_groups"` // NUMA node -> GPU indexes
}

// TopologyGPU is one GPU's place in NodeTopology.
type TopologyGPU struct {
	Index       int    `json:"index"`
	CPUAffinity string `json:"cpu_affinity"` // e.g. "0-23"
	NUMANode    int    `json:"numa_node"`    // -1 when unknown
}

// TopologyLink is the link between two GPUs: the probe's raw text ("NV12",
// "SYS", "PIX"), its class (topoparse.LinkClass, higher is faster) and the
// NVLink bond count for NV#.
type TopologyLink struct {
	Raw         string `json:"raw"`
	Class       int    `json:"class"`
	NVLinkBonds int    `json:"nvlink_bonds"`
}

// BenchmarkLongTail is a benchmark point's long-tail facts: every percentile
// of TTFT, TPOT, end-to-end latency and inter-token latency in milliseconds,
// keyed by percentile ("50", "99"), and the cache and speculative-decoding
// facts the run observed.
type BenchmarkLongTail struct {
	TTFTMS    map[string]float64 `json:"ttft_ms"`
	TPOTMS    map[string]float64 `json:"tpot_ms"`
	LatencyMS map[string]float64 `json:"latency_ms"`
	ITLMS     map[string]float64 `json:"itl_ms"`
	// ApproximateTokens marks token counts estimated from text length rather
	// than reported by the engine.
	ApproximateTokens   bool                   `json:"approximate_tokens"`
	KVHitRate           *float64               `json:"kv_hit_rate,omitempty"`
	CacheReportCoverage *float64               `json:"cache_report_coverage,omitempty"`
	PeakInputTokens     int                    `json:"peak_input_tokens"`
	Speculative         *BenchmarkSpeculative  `json:"speculative,omitempty"`
	Evidence            *metrics.PointEvidence `json:"evidence,omitempty"`
}

// BenchmarkSpeculative is speculative decoding's effect on one point.
type BenchmarkSpeculative struct {
	TokensPerIteration float64 `json:"tokens_per_iteration"`
	AcceptanceRate     float64 `json:"acceptance_rate"`
}

// AgentReject is one refused agent handshake, newest first in
// GET /api/v1/agent/rejects.
type AgentReject struct {
	TokenPrefix string    `json:"token_prefix"`
	Reason      string    `json:"reason"`
	Remote      string    `json:"remote"`
	At          time.Time `json:"at"`
}

// TerminalSession is one terminal session's audit record: metadata only, no
// terminal content is ever stored. Ended sessions also carry when and why
// they ended and how long they lasted.
type TerminalSession struct {
	ID          uint       `json:"id"`
	Username    string     `json:"username"`
	NodeUUID    string     `json:"node_uuid"`
	Target      string     `json:"target"`
	Ref         string     `json:"ref,omitempty"`
	ClientIP    string     `json:"client_ip"`
	StartedAt   time.Time  `json:"started_at"`
	BytesIn     int64      `json:"bytes_in"`
	BytesOut    int64      `json:"bytes_out"`
	Active      bool       `json:"active"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	DurationSec int64      `json:"duration_sec,omitempty"`
	CloseReason string     `json:"close_reason,omitempty"`
}
