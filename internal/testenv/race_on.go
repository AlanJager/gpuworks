//go:build race

package testenv

// RaceEnabled reports whether the test binary was built with the race detector.
// Throughput assertions relax their floor under -race because the shaper and
// yamux goroutines are heavily instrumented and the fixture itself, not the
// window config, becomes the bottleneck.
const RaceEnabled = true
