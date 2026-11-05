package debuglog

import (
	"log/slog"

	"gpuworks.ai/gpuworks/internal/logthrottle"
)

// NewLogger captures original records in bounded debug sessions before
// throttling the process log. The returned throttle runs only the ordinary
// log's summary timer via Run.
func NewLogger(inner slog.Handler, options logthrottle.Options) (*slog.Logger, *Tee, *logthrottle.Handler) {
	throttle := logthrottle.New(inner, options)
	tee := New(throttle)
	return slog.New(tee), tee, throttle
}
