package app

import (
	"context"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// shipSelfLogs drains the self-log collector into the log center: the agent's
// own warn+ output becomes searchable next to the node's and its instances'
// logs, under source=agent, with no instance UUID (it belongs to no
// deployment). Drain-and-emit is best-effort exactly like the records
// themselves — the collector drops on overflow, never blocking the logger.
func (n *NodeRuntime) shipSelfLogs(ctx context.Context) {
	n.runSelfLogShipper(ctx, time.Second)
}

// runSelfLogShipper is the loop with the tick injected for tests.
func (n *NodeRuntime) runSelfLogShipper(ctx context.Context, tick time.Duration) {
	if n.selfLog == nil {
		return
	}
	batch := make([]proto.LogLine, 0, 64)
	send := func() {
		if len(batch) == 0 {
			return
		}
		n.emit(proto.TLogs, proto.LogBatch{Lines: batch})
		// Fresh slice per batch: the connection may marshal asynchronously,
		// so ownership of the backing array transfers with the send.
		batch = make([]proto.LogLine, 0, 64)
	}
	drain := func() {
		for _, rec := range n.selfLog.Drain(256) {
			batch = append(batch, proto.LogLine{
				TimeMS: rec.TimeMS, Line: rec.Line,
				Level: rec.Level, Source: "agent",
			})
			if len(batch) >= 64 {
				send()
			}
		}
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			drain()
			send()
			return
		case <-ticker.C:
			drain()
			send()
		}
	}
}
