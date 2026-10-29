//go:build race

package batch

// raceEnabled: the race detector multiplies CPU and memory, so the scale
// test checks the structure (caps, write counts) and not the budgets.
const raceEnabled = true
