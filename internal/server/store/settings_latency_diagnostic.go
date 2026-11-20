package store

import (
	"context"
	"log/slog"
	"time"
)

// Record the target return before the synchronous notification call can wait.
// Successful fast returns are checkpoints too; neither phase asserts delivery,
// and a transaction error does not establish COMMIT or ROLLBACK outcome.
func (s *Store) logSettingWriteDiagnostic(ctx context.Context, phase string, started time.Time, err error) {
	if ctx == nil {
		ctx = context.Background() // Only diagnostic context; never replace work's context.
	}
	log := s.clusterGrantDiagnosticLogger()
	if !log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	log.LogAttrs(ctx, slog.LevelDebug, "setting write diagnostic",
		slog.String("operation", "setting_write"), slog.String("phase", phase),
		clusterGrantTiming("elapsed_ms", time.Since(started)),
		slog.String("err_class", clusterGrantErrorClass(err)),
		slog.String("caller_context_class", clusterGrantErrorClass(ctx.Err())))
}
