package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/pdverify"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/proxy"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// pdVerifyAdapter backs pdverify's interfaces with the store, the gateway
// and the metrics store.
type pdVerifyAdapter struct {
	store *store.Store
	proxy *proxy.Proxy
	tsdb  tsdb.Store
}

// runPDVerifier verifies PD handoffs until ctx ends, waking on instance
// changes as well as on its interval.
func runPDVerifier(ctx context.Context, st *store.Store, px *proxy.Proxy, metrics tsdb.Store) {
	adapter := &pdVerifyAdapter{store: st, proxy: px, tsdb: metrics}
	wake := make(chan struct{}, 1)
	verifier := &pdverify.Verifier{Source: adapter, Prober: adapter, Counters: adapter, Recorder: adapter, Settle: 25 * time.Second, Wake: wake}
	if st.Bus != nil {
		sub := st.Bus.Subscribe(64, "instance")
		go func() {
			defer sub.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case <-sub.C():
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	verifier.Run(ctx)
}

// Pending lists the running, awake prefill instances of active PD
// deployments that have a ready decoder and no verification on their own
// timeline yet. A rebuilt instance has a new UUID, so it is verified again.
func (a *pdVerifyAdapter) Pending(context.Context) ([]pdverify.Candidate, error) {
	deps, err := a.store.ListDeployments()
	if err != nil {
		return nil, err
	}
	var out []pdverify.Candidate
	for _, dep := range deps {
		if dep.State != store.DeployActive || pdPlacement(dep.Placement) == nil {
			continue
		}
		insts, err := a.store.InstancesForDeployment(dep.ID)
		if err != nil {
			return nil, err
		}
		// A multi-node unit is verified through its leader; its followers
		// serve nothing themselves.
		// planning.InstanceAsleep rather than the reported level: an instance
		// under a sleep order its engine has not carried out is already refused
		// by the gateway, so verifying through it would prove nothing.
		ready := func(inst store.Instance) bool {
			return inst.State == proto.InstRunning && !planning.InstanceAsleep(&inst) && !inst.Headless
		}
		decodeReady := false
		for _, inst := range insts {
			if inst.Role == pdmode.RoleDecode && ready(inst) {
				decodeReady = true
			}
		}
		if !decodeReady {
			continue
		}
		for _, inst := range insts {
			if inst.Role != pdmode.RolePrefill || !ready(inst) || a.verified(inst.UUID) {
				continue
			}
			out = append(out, pdverify.Candidate{DeploymentID: dep.ID, PrefillUUID: inst.UUID, NetworkType: recordedNetworkType(inst)})
		}
	}
	return out, nil
}

func (a *pdVerifyAdapter) verified(uuid string) bool {
	events, err := a.store.InstanceEventsByUUID(uuid, 0)
	if err != nil {
		return false
	}
	for _, ev := range events {
		if pdverify.IsConclusion(ev.Code) {
			return true
		}
	}
	return false
}

func (a *pdVerifyAdapter) Probe(ctx context.Context, deploymentID uint, prefillUUID string) (pdverify.Outcome, error) {
	handoff, err := a.proxy.VerifyPDHandoff(ctx, deploymentID, prefillUUID)
	if err != nil {
		return pdverify.Outcome{}, err
	}
	return pdverify.Outcome{
		DecodeUUID:  handoff.DecodeUUID,
		DecodeNodes: a.rankNodes(deploymentID, handoff.DecodeUUID, handoff.DecodeNodeUUID), PrefillNodes: a.rankNodes(deploymentID, prefillUUID, handoff.PrefillNode),
		Bytes: handoff.Bytes, Unmeasured: !handoff.Measured, Failed: handoff.Failed, Reason: handoff.Reason, StartedAt: handoff.StartedAt, EndedAt: handoff.EndedAt,
	}, nil
}

// rankNodes is the nodes an instance's ranks run on: its own node, and for a
// multi-node leader the nodes of the followers in its group.
func (a *pdVerifyAdapter) rankNodes(deploymentID uint, uuid, node string) []string {
	out := []string{node}
	insts, err := a.store.InstancesForDeployment(deploymentID)
	if err != nil {
		return out
	}
	group := ""
	for _, inst := range insts {
		if inst.UUID == uuid {
			group = inst.DistGroup
		}
	}
	if group == "" {
		return out
	}
	for _, inst := range insts {
		if inst.Headless && inst.DistGroup == group && inst.State == proto.InstRunning {
			if n, err := a.store.NodeByID(inst.NodeID); err == nil {
				out = append(out, n.UUID)
			}
		}
	}
	return out
}

// RDMAReceived integrates a node's RDMA receive rate over a span.
func (a *pdVerifyAdapter) RDMAReceived(_ context.Context, nodeUUID string, from, to time.Time) (float64, bool, error) {
	if a.tsdb == nil {
		return 0, false, nil
	}
	const step = 10 * time.Second
	series, err := a.tsdb.QueryRange("gpuworks_node_rdma_rx_bytes_per_sec", map[string]string{"node": nodeUUID}, from.UnixMilli(), to.UnixMilli(), step.Milliseconds())
	if err != nil || len(series) == 0 {
		return 0, false, err
	}
	total := 0.0
	for _, s := range series {
		for _, p := range s.Points {
			total += p.Value * step.Seconds()
		}
	}
	return total, true, nil
}

func (a *pdVerifyAdapter) Record(prefillUUID, code string, params map[string]any) {
	inst, err := a.store.InstanceByUUID(prefillUUID)
	if err != nil {
		return
	}
	a.store.AddInstanceEventCode(inst, pdverify.EventKind, code, params)
}

// recordedNetworkType is the compute network type an instance started on.
func recordedNetworkType(inst store.Instance) string {
	network, _ := planning.ReadScheduleRecord(inst.ScheduleExpl).ComputeNetwork()
	return network.Type
}
