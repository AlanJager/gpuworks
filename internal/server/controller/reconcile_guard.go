package controller

import (
	"context"
	"fmt"
	"runtime/debug"
)

// reconcileDeploymentGuarded contains a reconcile panic inside the deployment
// that caused it.
//
// The worker is the only consumer of the queue, so an unrecovered panic here
// does not fail one deployment - it takes the whole server process down. The
// supervisor restarts it, the same deployment is primed onto the queue again,
// and the server crash-loops until an operator deletes that deployment. Turning
// the panic into this deployment's own error keeps it on the existing failure
// path (logged, retried on the next event) and leaves every other deployment
// reconciling. The stack is logged because a panic that never reaches the
// process' own crash output would otherwise leave no trace of where it came
// from.
func (c *Controller) reconcileDeploymentGuarded(ctx context.Context, id uint) (err error) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("reconcile deployment panicked", "id", id, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("deployment %d reconcile panicked: %v", id, r)
		}
	}()
	return c.reconcileDeployment(ctx, id)
}
