package collector

import "sort"

// subUint64 treats a counter that moved backwards (reboot, wrap, a swapped
// disk) as no movement for this tick rather than a huge bogus rate; the next
// tick recovers a normal delta once the counter has grown past its base again.
func subUint64(cur, prev uint64) uint64 {
	if cur < prev {
		return 0
	}
	return cur - prev
}

// sortedKeys returns m's keys in order, so series come out in a stable order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
