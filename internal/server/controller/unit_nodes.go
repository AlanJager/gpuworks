package controller

import (
	"errors"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// sighted is a node as the offline clocks read it: last seen no earlier than
// the moment this server began watching (Controller.watchingSince).
func (c *Controller) sighted(node *store.Node) *store.Node {
	if node == nil || node.LastSeenAt == nil || c.watchingSince.IsZero() || !node.LastSeenAt.Before(c.watchingSince) {
		return node
	}
	seen := *node
	at := c.watchingSince
	seen.LastSeenAt = &at
	return &seen
}

func (c *Controller) offlineGraceRemaining(node *store.Node, now time.Time) time.Duration {
	node = c.sighted(node)
	if c.offlineGrace <= 0 || node == nil || node.LastSeenAt == nil {
		return 0
	}
	remaining := node.LastSeenAt.Add(c.offlineGrace).Sub(now)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// pushRemoveOnline asks the online nodes to remove these instances and skips
// the offline ones: an offline node cannot acknowledge a removal, so its rows
// stay removing (settleRetiring) and are pushed again once it reconnects.
func (c *Controller) pushRemoveOnline(insts []store.Instance, removed []string) error {
	byNode := map[string][]string{}
	for _, uuid := range removed {
		for _, inst := range insts {
			if inst.UUID != uuid {
				continue
			}
			if node, err := c.store.NodeByID(inst.NodeID); err == nil {
				byNode[node.UUID] = append(byNode[node.UUID], uuid)
			}
		}
	}
	for nodeUUID, ids := range byNode {
		if !c.hub.Online(nodeUUID) {
			c.log.Info("distributed cleanup deferred to orphan reconciliation", "node", nodeUUID, "instances", ids)
			continue
		}
		if err := c.pushRemoveInstances(nodeUUID, ids); err != nil {
			return err
		}
	}
	return nil
}

// deploymentNodes loads the deployment's stored nodes in order. A node that no
// longer exists is left out; any other read failure fails the pass, since
// matching instances against a partial node set would read healthy replicas
// as unclaimed and drain them.
func (c *Controller) deploymentNodes(dep *store.Deployment) ([]*store.Node, error) {
	ids := dep.NodeIDList()
	out := make([]*store.Node, 0, len(ids))
	for _, id := range ids {
		n, err := c.store.NodeByID(id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("deployment %d node %d: %w", dep.ID, id, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// matchDeploymentRequirements rechecks the deployment's node requirements
// against a node the controller is about to schedule on. The api checks them
// when the request arrives; a label edit, a card swap or a network change in
// between must stop the launch rather than deploy onto a node the deployment
// excludes. Both the single-node and the distributed reconcile path call
// this, so they cannot drift apart. A node's networks are resolved only when
// a requirement names one; a resolution that cannot be read leaves every
// network requirement unmet.
func (c *Controller) matchDeploymentRequirements(deployment *unitDeployment, node *store.Node) error {
	resolve := func([]store.Node) (netplan.Resolution, error) { return c.networks.NodeNetworks(node.ID) }
	facts := planning.LoadNodeFacts(c.store, resolve, []store.Node{*node}, deployment.requirements)
	if err := planning.MatchRequirements(node, facts, deployment.requirements); err != nil {
		return fmt.Errorf("deployment %d: %w", deployment.dep.ID, err)
	}
	return nil
}

func (c *Controller) pushRemoveByNode(insts []store.Instance, removed []string) error {
	byNode := map[string][]string{}
	for _, uuid := range removed {
		for _, inst := range insts {
			if inst.UUID != uuid {
				continue
			}
			if n, err := c.store.NodeByID(inst.NodeID); err == nil {
				byNode[n.UUID] = append(byNode[n.UUID], uuid)
			}
		}
	}
	for node, ids := range byNode {
		if err := c.pushRemoveInstances(node, ids); err != nil {
			return err
		}
	}
	return nil
}
