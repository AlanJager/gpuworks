package api

// A zero inventory capacity is an absent measurement, not a zero-byte card.
// Expose this as a derived DTO fact for both node and fleet readers; it is not
// persisted or used to replace the scheduler's capacity checks.
func gpuMemoryCapacityKnown(total int64) bool { return total > 0 }
