// Package parentwatch ends a child process's work once the process that
// started it is gone, however that one ended: the experience environment
// beside a server, and its simulated nodes, must not outlive their starter
// and hold its ports.
package parentwatch

import (
	"context"
	"os"
	"time"
)

// interval is how often the parent is looked for.
const interval = 2 * time.Second

// Follow calls gone once this process's parent is no longer pid (it exited,
// and the process was reparented), or returns when ctx ends first.
func Follow(ctx context.Context, pid int, gone func()) {
	follow(ctx, pid, gone, os.Getppid, interval)
}

func follow(ctx context.Context, pid int, gone func(), getppid func() int, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if getppid() != pid {
			gone()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
