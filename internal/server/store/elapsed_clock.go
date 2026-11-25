package store

import "time"

// elapsedClock reads the clock for measuring elapsed time in memory. Its
// value keeps the monotonic reading, so a step of the wall clock neither
// shortens nor stretches an age measured with it; time.Now().UTC() drops
// that reading. A value from it must never be written to the database or
// serialized. This file is the one exemption of scripts/check_time_policy.sh,
// which names it; nothing else belongs here.
func elapsedClock() time.Time { return time.Now() }
