package cli

import (
	"log/slog"
	"os"
	"runtime"
)

// serverMaxProcs caps how many OS threads run the server's Go code at once.
// On a host with many logical CPUs (two sockets, several NUMA nodes) the
// runtime's schedulers and background GC workers spread over every one of
// them; under gateway load that costs 2.5 to 3.5 times the CPU per request
// for the same throughput. Peak server load measured at the scale target
// (gateway, fleet churn and a server-origin evaluation together) needs about
// 15 cores, so 32 leaves room above it.
const serverMaxProcs = 32

// maxProcsDecision says what the server does with GOMAXPROCS: lower it to
// want, or keep current. An operator's own GOMAXPROCS is always kept, and so
// is a value already at or below the cap (a container's CPU limit sets one).
func maxProcsDecision(current int, operatorSet bool, limit int) (want int, lower bool) {
	if operatorSet || limit <= 0 || current <= limit {
		return current, false
	}
	return limit, true
}

// capServerMaxProcs applies maxProcsDecision to this process. Lowering it
// turns off the runtime's own tracking of a container CPU limit (Go 1.25),
// which only ever matters if that limit is changed while the server runs.
func capServerMaxProcs(log *slog.Logger) {
	_, operatorSet := os.LookupEnv("GOMAXPROCS")
	current := runtime.GOMAXPROCS(0)
	want, lower := maxProcsDecision(current, operatorSet, serverMaxProcs)
	if !lower {
		return
	}
	runtime.GOMAXPROCS(want)
	log.Info("limited the server's Go processors; set GOMAXPROCS to choose another number",
		"gomaxprocs", want, "default", current, "cpus", runtime.NumCPU())
}
