package agentupgrade

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Gateway is the inference gateway as an upgrade uses it. An upgrade restarts
// the agent a node's instances are reached through, so before it does it asks
// the gateway to send their deployments' new requests to other replicas, and
// waits for the requests on the node to finish. The streaming proxy
// implements it; the app wires it in.
type Gateway interface {
	// ServedElsewhere reports whether the gateway would route the
	// deployment's requests to a replica other than instances right now.
	ServedElsewhere(deploymentID uint, instances []string) bool
	// Avoid prefers the deployment's other replicas over instances for new
	// requests until release is called. It never refuses a request: when no
	// other replica can take one, the avoided instances still do.
	Avoid(deploymentID uint, instances []string) (release func())
	// InstancesInFlight is how many requests the gateway is serving on the
	// instances.
	InstancesInFlight(instances []string) int
}

// ReasonDrainIncomplete is an attempt dispatched while requests were still
// running on its node's drained instances: the bound ran out, and the agent's
// restart cut them.
const ReasonDrainIncomplete = "drain_incomplete"

// A node's upgrade waits, first, for the requests running on it to finish,
// while the gateway sends the new ones to its deployments' other replicas.
// Only deployments the gateway serves on another node right now take part:
// one that has no other replica awake on an online node keeps serving on this
// node until the restart, exactly as before the wait existed, so the wait
// never makes a model less available than the restart alone would. The
// gateway's own view decides (Gateway.ServedElsewhere), and the gateway
// re-decides per request (Gateway.Avoid is a preference, not a refusal), so a
// sibling that falls asleep or goes offline mid-wait only stops the wait for
// that deployment.
//
// The wait is bounded (DrainTimeout): past the bound the upgrade goes ahead
// and the attempt says so (ReasonDrainIncomplete). The preference stays on
// through the dispatch, while the binary transfers and the agent restarts,
// and ends when the attempt leaves its active phases or its dispatch fails.
// It lives only in memory, so a server that restarts routes as before; a
// draining attempt it finds dispatched nothing and goes back to the queue
// (store.RecoverAgentUpgrades).

// drainSet is, for one node, the instances of each deployment the upgrade
// waits for.
type drainSet map[uint][]string

// planDrain is what the node's upgrade waits for: its routable instances of
// deployments the gateway serves elsewhere. It only reads, so a failure leaves
// the attempt queued.
func (s *Service) planDrain(node *store.Node) (drainSet, error) {
	instances, err := s.st.InstancesForNode(node.ID)
	if err != nil {
		return nil, err
	}
	set := drainSet{}
	for _, i := range instances {
		if i.State == proto.InstRunning && !i.Headless && i.APIPort > 0 {
			set[i.DeploymentID] = append(set[i.DeploymentID], i.UUID)
		}
	}
	for deploymentID, list := range set {
		if !s.cfg.Gateway.ServedElsewhere(deploymentID, list) {
			delete(set, deploymentID)
		}
	}
	return set, nil
}

// drain claims the attempt into the draining stage, prefers other replicas
// over the node's instances and waits for what runs on them. It reports
// whether the attempt may be dispatched; when it may not, the attempt has
// already been moved to where it belongs (queued, cancelled) or was never
// claimed.
func (s *Service) drain(ctx context.Context, item *store.AgentUpgradeItem, set drainSet) (bool, error) {
	claimed, err := s.st.ClaimAgentUpgradeDrain(item.ID, s.now(), s.heldSince())
	if err != nil || !claimed {
		return false, err
	}
	s.notify()
	s.holds.take(item.ID, s.cfg.Gateway, set)
	cut, err := s.waitDrain(ctx, item.ID, item.RunID, set)
	if err != nil {
		// A server that is stopping leaves the stage for startup recovery to
		// requeue; the next pass of a runner that goes on does it instead
		// (requeueAbandonedDrains).
		s.holds.release(item.ID)
		return false, err
	}
	if run, err := s.st.AgentUpgradeRun(item.RunID); err == nil && run.StopRequestedAt != nil {
		// The operator stopped the batch while this node drained: it had not
		// started.
		s.holds.release(item.ID)
		_, err = s.st.UpdateAgentUpgradeItem(item.ID, []string{"draining"}, map[string]any{"phase": "cancelled", "reason": "stopped", "finished_at": s.now(), "started_at": nil})
		s.notify()
		return false, err
	}
	if cut > 0 {
		item.Reason = ReasonDrainIncomplete
		s.cfg.Logger.Warn("agent upgrade dispatched with requests still running on the node", "node", item.NodeUUID, "in_flight", cut, "drain", s.cfg.DrainTimeout.String())
	}
	return true, nil
}

// waitDrain waits for what set's instances serve, at most DrainTimeout long,
// looking every DrainPoll. A deployment the gateway no longer serves elsewhere
// leaves the wait (its instances keep serving to the restart), and an
// operator's stop ends it within one look. It returns what was still in flight
// when it stopped waiting.
func (s *Service) waitDrain(ctx context.Context, itemID, runID uint, set drainSet) (int, error) {
	deadline := s.now().Add(s.cfg.DrainTimeout)
	ticker := time.NewTicker(s.cfg.DrainPoll)
	defer ticker.Stop()
	for {
		inFlight := 0
		for deploymentID, list := range set {
			if !s.cfg.Gateway.ServedElsewhere(deploymentID, list) {
				s.holds.drop(itemID, deploymentID)
				delete(set, deploymentID)
				continue
			}
			inFlight += s.cfg.Gateway.InstancesInFlight(list)
		}
		if inFlight == 0 || !s.now().Before(deadline) {
			return inFlight, nil
		}
		if run, err := s.st.AgentUpgradeRun(runID); err == nil && run.StopRequestedAt != nil {
			return inFlight, nil
		}
		select {
		case <-ctx.Done():
			return inFlight, ctx.Err()
		case <-ticker.C:
		}
	}
}

// requeueAbandonedDrains returns an attempt found in the draining stage with no
// drain held here to the queue. The drain runs inside one pass, so anything
// still in that stage when the next pass starts dispatched nothing; leaving it
// there would hold the platform's slot — and the batch's next node, a new batch
// and a server upgrade — until the server restarted.
func (s *Service) requeueAbandonedDrains(items []store.AgentUpgradeItem) error {
	for index := range items {
		i := &items[index]
		if i.Phase != "draining" || s.holds.held(i.ID) {
			continue
		}
		changed, err := s.st.RequeueAgentUpgradeDrain(i.ID)
		if err != nil {
			return err
		}
		if changed {
			i.Phase, i.StartedAt, i.DeadlineAt, i.BaselineSessionID = "queued", nil, nil, ""
			s.notify()
		}
	}
	return nil
}

// maxDrainRequeues is how many times an attempt goes back to the queue from
// its drain, its node having reconnected meanwhile, before its next dispatch
// stops waiting: a node whose agent reconnects more often than a drain lasts
// would otherwise drain forever and never upgrade.
const maxDrainRequeues = 2

// requeueDrain returns a drained attempt that cannot be dispatched to the
// queue, ending its preference; the next pass judges it again.
func (s *Service) requeueDrain(item store.AgentUpgradeItem) error {
	s.holds.release(item.ID)
	s.holds.noteRequeue(item.ID)
	_, err := s.st.RequeueAgentUpgradeDrain(item.ID)
	s.notify()
	return err
}

// dispatchDrained moves a drained attempt to transferring, with the session,
// start and deadline its dispatch is judged by: an agent's report from before
// now (observeEvent) is not about this dispatch.
func (s *Service) dispatchDrained(item *store.AgentUpgradeItem, session string, now, deadline time.Time) (bool, error) {
	values := map[string]any{"phase": "transferring", "baseline_session_id": session, "started_at": now, "deadline_at": deadline}
	if item.Reason != "" {
		values["reason"] = item.Reason
	}
	changed, err := s.st.UpdateAgentUpgradeItem(item.ID, []string{"draining"}, values)
	if err != nil || !changed {
		// Not dispatched: a store failure leaves the stage for the next pass
		// to requeue, and an attempt that already left it is not this pass's.
		s.holds.release(item.ID)
		return false, err
	}
	item.Phase, item.BaselineSessionID, item.StartedAt, item.DeadlineAt = "transferring", session, &now, &deadline
	s.notify()
	return true, nil
}
