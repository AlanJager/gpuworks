//go:build !race

package api

// raceEnabled reports whether this test binary was built with the race
// detector, under which the fleet-scale test logs its times instead of
// holding them to budgets (overBudget).
const raceEnabled = false
