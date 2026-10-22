package controller

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// cacheCopyClaims is a derived reverse index of deployments preparing a cache.
// It never owns files: the durable model-file intent and the agent's writer
// slot remain authoritative. Only the controller mutates cache intents — its
// worker withdraws them, RetryCacheCopy restarts failed ones — and each change
// is a compare-and-swap on the intent's transfer ID.
type cacheCopyClaims struct {
	mu           sync.Mutex
	byDeployment map[uint]map[store.FileRef]struct{}
	byFile       map[store.FileRef]map[uint]struct{}
}

func (c *cacheCopyClaims) claim(deployment uint, ref store.FileRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byDeployment == nil {
		c.byDeployment = map[uint]map[store.FileRef]struct{}{}
		c.byFile = map[store.FileRef]map[uint]struct{}{}
	}
	if c.byDeployment[deployment] == nil {
		c.byDeployment[deployment] = map[store.FileRef]struct{}{}
	}
	if c.byFile[ref] == nil {
		c.byFile[ref] = map[uint]struct{}{}
	}
	c.byDeployment[deployment][ref] = struct{}{}
	c.byFile[ref][deployment] = struct{}{}
}

func (c *cacheCopyClaims) release(deployment uint) []store.FileRef {
	return c.releaseOutsideNodes(deployment, nil)
}

// releaseOutsideNodes narrows restart claims only after placement is resolved.
// Keep every dependency on retained nodes: readiness may stop at the first
// unfinished model, so a pass need not have visited the remaining components.
func (c *cacheCopyClaims) releaseOutsideNodes(deployment uint, nodes []uint) []store.FileRef {
	retained := make(map[uint]struct{}, len(nodes))
	for _, node := range nodes {
		retained[node] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var unused []store.FileRef
	for ref := range c.byDeployment[deployment] {
		if _, keep := retained[ref.NodeID]; keep {
			continue
		}
		delete(c.byDeployment[deployment], ref)
		delete(c.byFile[ref], deployment)
		if len(c.byFile[ref]) == 0 {
			delete(c.byFile, ref)
			unused = append(unused, ref)
		}
	}
	if len(c.byDeployment[deployment]) == 0 {
		delete(c.byDeployment, deployment)
	}
	return unused
}

// seedCacheCopyClaims runs once before the worker starts, reusing the startup
// deployment snapshot. Unknown dynamic placement conservatively retains copies
// of the same dependency until reconciliation establishes an actual placement.
func (c *Controller) seedCacheCopyClaims(deployments []store.Deployment) error {
	c.cacheCopies = cacheCopyClaims{}
	rows, err := c.store.UnfinishedCacheCopies()
	if err != nil {
		return err
	}
	byModel := map[uint][]store.ModelFile{}
	for _, row := range rows {
		byModel[row.ModelID] = append(byModel[row.ModelID], row)
	}
	for i := range deployments {
		dep := &deployments[i]
		ids, err := planning.DeploymentModelIDs(dep)
		if err != nil {
			c.log.Warn("invalid deployment cache dependencies", "deployment", dep.ID, "err", err)
			continue
		}
		// NodeIDList preserves the legacy primary-node fallback, including
		// zero for an automatic placement that has not selected a node.
		nodes := slices.DeleteFunc(dep.NodeIDList(), func(id uint) bool { return id == 0 })
		for _, id := range ids {
			for _, row := range byModel[id] {
				if len(nodes) == 0 || slices.Contains(nodes, row.NodeID) {
					c.cacheCopies.claim(dep.ID, store.FileRef{ModelID: row.ModelID, NodeID: row.NodeID, StorageID: row.StorageID})
				}
			}
		}
	}
	return nil
}

// withdrawCacheCopies does not wait for a potentially hung filesystem. It
// fences late reports in storage and sends a withdrawn desired state. The
// agent retains its slot until I/O returns; a subsequent claimant gets a new
// intent and cannot bypass that slot. Partial data remains available to resume.
func (c *Controller) withdrawCacheCopies(ctx context.Context, deployment uint) error {
	return c.withdrawUnclaimedCopies(ctx, deployment, c.cacheCopies.release(deployment))
}

func (c *Controller) retainCacheCopyNodes(ctx context.Context, deployment uint, nodes []uint) error {
	return c.withdrawUnclaimedCopies(ctx, deployment, c.cacheCopies.releaseOutsideNodes(deployment, nodes))
}

func (c *Controller) withdrawUnclaimedCopies(ctx context.Context, deployment uint, refs []store.FileRef) error {
	for i, ref := range refs {
		if err := c.withdrawCacheCopy(ctx, ref); err != nil {
			// Preserve every unprocessed claim for the next reconciliation.
			for _, pending := range refs[i:] {
				c.cacheCopies.claim(deployment, pending)
			}
			return err
		}
	}
	return nil
}

func (c *Controller) withdrawCacheCopy(ctx context.Context, ref store.FileRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := c.store.ModelFileAt(ref)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if file.Ready() {
		return nil
	}
	// Completed/running users are represented by durable instance reads too.
	if err := c.store.CheckModelFileRowEviction(file.ID); err != nil {
		if errors.Is(err, store.ErrReferenced) {
			return nil
		}
		return err
	}
	if err := c.store.SuspendCacheCopy(file.ID, file.TransferID); err != nil {
		return err
	}
	file, err = c.store.ModelFileByID(file.ID)
	if err != nil {
		return err
	}
	node, err := c.store.NodeByID(file.NodeID)
	if err != nil {
		return err
	}
	model, err := c.store.GetModel(file.ModelID)
	if err != nil {
		return err
	}
	return c.pushModelFile(node, file, model)
}

// primeCacheCopyClaims retries transient startup reads before admitting work.
// Proceeding with an incomplete index could cancel a copy another deployment
// still needs; abandoning Run would silently leave the controller inactive.
func (c *Controller) primeCacheCopyClaims(ctx context.Context) bool {
	for ctx.Err() == nil {
		deps, err := c.store.ListDeployments()
		if err == nil {
			err = c.seedCacheCopyClaims(deps)
		}
		if err == nil {
			for _, dep := range deps {
				c.q.add(depKey(dep.ID))
			}
			return true
		}
		c.log.Warn("reconstruct cache copy claims", "err", err)
		timer := time.NewTimer(max(c.retryBackoff, time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}
