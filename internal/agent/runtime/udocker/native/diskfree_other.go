//go:build !linux && !darwin

package native

// availableBytes is unavailable on this platform; callers treat -1 as
// "unknown" and skip optimizations that trade disk for time.
func availableBytes(string) int64 { return -1 }
