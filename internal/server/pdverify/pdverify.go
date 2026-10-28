// Package pdverify checks, once per prefill instance, that a PD deployment's
// KV handoff really happens and over which transport (plan §4.7, ADR 0028).
// Configuration alone cannot answer that: KV was measured to fall back to
// Ethernet silently when a container could not see the RDMA devices, at half
// the throughput and with no error anywhere.
//
// For each prefill instance that has not been verified since it started, the
// verifier sends one small request through the two stages pinned to it,
// reads how many bytes the decoder pulled, and compares that with the RDMA
// traffic the decoder's node counted over the same span. The conclusion is an
// instance event; nothing is stored beside the timeline.
//
// The package holds the decision only. Reading rows, sending the request and
// reading node counters come in through the interfaces below, which the app
// layer implements.
package pdverify

import (
	"context"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/server/netplan"
)

// Event codes recorded on the prefill instance's timeline.
const (
	// EventVerified: KV moved. Params: transport (rdma | tcp | local),
	// bytes, decode.
	EventVerified = "PD_HANDOFF_VERIFIED"
	// EventNotRDMA: KV moved, but not over RDMA although the instances sit
	// on an RDMA compute network. Params: bytes, rdma_bytes, decode.
	EventNotRDMA = "PD_HANDOFF_NOT_RDMA"
	// EventFailed: no KV moved. Params: reason, decode.
	EventFailed = "PD_HANDOFF_FAILED"
)

// IsConclusion reports whether a timeline event code is one of the
// verifier's conclusions: an instance that carries one has been verified.
func IsConclusion(code string) bool {
	return code == EventVerified || code == EventNotRDMA || code == EventFailed
}

// EventKind is the timeline kind the verifier writes.
const EventKind = "pd"

// Transports a verified handoff used.
const (
	TransportRDMA  = "rdma"
	TransportTCP   = "tcp"
	TransportLocal = "local"
	// TransportUnmeasured: the handoff succeeded but the engine counts no
	// transferred bytes to say how.
	TransportUnmeasured = "unmeasured"
)

// Candidate is a prefill instance waiting for verification.
type Candidate struct {
	DeploymentID uint
	PrefillUUID  string
	// NetworkType is the compute network type the instance started on (a
	// netplan type, or empty for the management network).
	NetworkType string
}

// Outcome is what one verification handoff observed.
type Outcome struct {
	DecodeUUID string
	// DecodeNodes and PrefillNodes are the nodes of every rank of the two
	// instances: a multi-node instance moves KV from each of its ranks.
	DecodeNodes  []string
	PrefillNodes []string
	Bytes        float64
	Unmeasured   bool
	Failed       bool
	Reason       string
	StartedAt    time.Time
	EndedAt      time.Time
}

// Source lists the prefill instances to verify.
type Source interface {
	Pending(ctx context.Context) ([]Candidate, error)
}

// Prober runs one verification handoff.
type Prober interface {
	Probe(ctx context.Context, deploymentID uint, prefillUUID string) (Outcome, error)
}

// Counters reads how many bytes a node's RDMA ports received over a span;
// false when the node reports no RDMA counters.
type Counters interface {
	RDMAReceived(ctx context.Context, nodeUUID string, from, to time.Time) (float64, bool, error)
}

// Recorder writes the conclusion on the prefill instance's timeline.
type Recorder interface {
	Record(prefillUUID, code string, params map[string]any)
}

// Verifier runs the checks.
type Verifier struct {
	Source   Source
	Prober   Prober
	Counters Counters
	Recorder Recorder
	Log      *slog.Logger
	// Interval is how often pending instances are looked for; Settle is how
	// long to wait after a handoff before reading node counters, which the
	// agents report every 10 s.
	Interval time.Duration
	Settle   time.Duration
	// Wake, when set, starts a check before the next interval: the app
	// signals it when instances change, so a prefill instance that just
	// became ready is verified promptly.
	Wake <-chan struct{}
	// WakeGap is the least time between two passes a wake starts. Instance
	// changes arrive in storms (a fleet redeploying fires thousands), each
	// pass reads every deployment, and a prefill instance waits Settle
	// before it is checked anyway: one pass per gap covers the storm.
	WakeGap time.Duration
	sleep   func(context.Context, time.Duration) bool
}

// defaultWakeGap is WakeGap when unset.
const defaultWakeGap = 5 * time.Second

// Run checks pending instances every Interval until ctx ends.
func (v *Verifier) Run(ctx context.Context) {
	interval := v.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		v.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-v.Wake:
			gap := v.WakeGap
			if gap <= 0 {
				gap = defaultWakeGap
			}
			if !v.wait(ctx, gap) {
				return
			}
		}
	}
}

// Tick verifies every pending instance once.
func (v *Verifier) Tick(ctx context.Context) {
	pending, err := v.Source.Pending(ctx)
	if err != nil {
		v.log().Warn("list PD instances to verify", "err", err)
		return
	}
	for _, c := range pending {
		if ctx.Err() != nil {
			return
		}
		outcome, err := v.Prober.Probe(ctx, c.DeploymentID, c.PrefillUUID)
		if err != nil {
			// Not routable yet (the route table has not seen it, no decoder
			// is ready): try again on a later tick, record nothing.
			v.log().Debug("PD handoff check deferred", "deployment", c.DeploymentID, "prefill", c.PrefillUUID, "err", err)
			continue
		}
		code, params := v.conclude(ctx, c, outcome)
		v.Recorder.Record(c.PrefillUUID, code, params)
	}
}

// conclude turns an outcome into the timeline entry.
func (v *Verifier) conclude(ctx context.Context, c Candidate, o Outcome) (string, map[string]any) {
	if !o.Failed && o.Unmeasured {
		return EventVerified, map[string]any{"transport": TransportUnmeasured, "decode": o.DecodeUUID}
	}
	if o.Failed || o.Bytes <= 0 {
		reason := o.Reason
		if reason == "" {
			reason = "no_transfer"
		}
		return EventFailed, map[string]any{"reason": reason, "decode": o.DecodeUUID}
	}
	params := map[string]any{"bytes": int64(o.Bytes), "decode": o.DecodeUUID}
	if sameSingleNode(o.DecodeNodes, o.PrefillNodes) {
		params["transport"] = TransportLocal
		return EventVerified, params
	}
	if netplan.IsAccelerator(c.NetworkType) {
		// The KV rides the cards' own ports, whose traffic no host counter
		// sees, so the handoff is proven but its transport is not.
		params["transport"] = TransportUnmeasured
		return EventVerified, params
	}
	if c.NetworkType != netplan.TypeHostRDMA {
		params["transport"] = TransportTCP
		return EventVerified, params
	}
	if !v.wait(ctx, v.Settle) {
		params["transport"] = TransportTCP
		return EventVerified, params
	}
	// Every decode rank pulls its share, so the RDMA received on all of
	// their nodes together is compared with what the decoder counted.
	received, known := 0.0, true
	var err error
	for _, node := range unique(o.DecodeNodes) {
		got, ok, nodeErr := v.Counters.RDMAReceived(ctx, node, o.StartedAt.Add(-10*time.Second), o.EndedAt.Add(v.Settle))
		if nodeErr != nil {
			err = nodeErr
		}
		received += got
		known = known && ok
	}
	if err == nil && known && len(o.DecodeNodes) > 0 && received >= o.Bytes/2 {
		params["transport"] = TransportRDMA
		return EventVerified, params
	}
	params["rdma_bytes"] = int64(received)
	return EventNotRDMA, params
}

// sameSingleNode reports whether every rank of both instances runs on one
// node, so the KV never left it.
func sameSingleNode(decode, prefill []string) bool {
	nodes := unique(append(append([]string(nil), decode...), prefill...))
	return len(nodes) == 1 && len(decode) > 0 && len(prefill) > 0
}

func unique(nodes []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, node := range nodes {
		if node != "" && !seen[node] {
			seen[node] = true
			out = append(out, node)
		}
	}
	return out
}

func (v *Verifier) wait(ctx context.Context, d time.Duration) bool {
	if v.sleep != nil {
		return v.sleep(ctx, d)
	}
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (v *Verifier) log() *slog.Logger {
	if v.Log == nil {
		return slog.Default()
	}
	return v.Log
}
