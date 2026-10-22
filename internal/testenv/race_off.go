//go:build !race

package testenv

// RaceEnabled reports whether the test binary was built with the race detector.
const RaceEnabled = false
