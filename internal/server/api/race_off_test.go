//go:build !race

package api

// raceEnabled reports whether this test binary was built with the race
// detector, which slows the fleet-scale budgets down several times.
const raceEnabled = false
