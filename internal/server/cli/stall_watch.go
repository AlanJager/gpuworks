package cli

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/stallwatch"
	"gpuworks.ai/gpuworks/internal/version"
)

// stallReportAfter reads server.yaml's log.stall_report_after: a duration,
// with 0 turning the reports off.
func stallReportAfter(configured string) (time.Duration, error) {
	after, err := time.ParseDuration(configured)
	if err != nil && configured == "0" {
		return 0, nil
	}
	if err != nil || after < 0 {
		return 0, fmt.Errorf("log.stall_report_after %q is not a duration such as 3s, or 0 to turn the reports off", configured)
	}
	return after, nil
}

// startStallWatch makes the server record its own stalls: a log write or a
// store operation that waits longer than after leaves every goroutine's
// stack in a report under the data directory's diagnostics, and one line in
// the log. Nothing is watched when after is zero. The returned function
// stops the watch.
func startStallWatch(log *slog.Logger, after time.Duration, dataDir string) func() {
	if after <= 0 {
		return func() {}
	}
	watch := stallwatch.New(stallwatch.Options{
		Threshold: after,
		Dir:       diagnostics.StallReportDir(dataDir),
		Log:       log,
		Header:    []string{"program: gpuworks server " + version.String()},
	})
	stallwatch.Use(watch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watch.Run(ctx)
	}()
	// Run itself gives up on a reporting goroutine that is stuck on the
	// disk (stallwatch.DefaultStopWait), so this wait is bounded.
	return func() {
		stallwatch.Use(nil)
		cancel()
		<-done
	}
}
