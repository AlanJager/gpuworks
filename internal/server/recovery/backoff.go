package recovery

import "time"

// Backoff returns initial*multiplier^(attempts-1), capped at max. Attempts less
// than one use the initial interval. The loop caps before multiplication so a
// corrupted attempt counter cannot overflow a duration.
func Backoff(attempts int, initial time.Duration, multiplier float64, max time.Duration) time.Duration {
	if initial <= 0 {
		initial = time.Second
	}
	if max <= 0 || max < initial {
		max = initial
	}
	if multiplier < 1 {
		multiplier = 1
	}
	delay := float64(initial)
	for step := 1; step < attempts; step++ {
		if delay >= float64(max)/multiplier {
			return max
		}
		delay *= multiplier
	}
	if delay >= float64(max) {
		return max
	}
	return time.Duration(delay)
}
