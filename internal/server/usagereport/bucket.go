package usagereport

import (
	"strings"
	"time"
)

// DurationBucket names the range a duration falls in, so a report says how
// long something took without carrying a timestamp.
func DurationBucket(d time.Duration) string {
	switch {
	case d < 30*time.Second:
		return "<30s"
	case d < 2*time.Minute:
		return "30s-2m"
	case d < 10*time.Minute:
		return "2m-10m"
	case d < 30*time.Minute:
		return "10m-30m"
	case d < 2*time.Hour:
		return "30m-2h"
	default:
		return ">2h"
	}
}

// SizeBucket names the range a byte count falls in.
func SizeBucket(n int64) string {
	const gib = 1 << 30
	switch {
	case n <= 0:
		return Unknown
	case n < gib:
		return "<1G"
	case n < 10*gib:
		return "1G-10G"
	case n < 100*gib:
		return "10G-100G"
	case n < 500*gib:
		return "100G-500G"
	default:
		return ">500G"
	}
}

// MajorMinor cuts a version to its first two numeric parts: "6.8.0-138-generic"
// is "6.8", "12.4.1" is "12.4", "v0.9.1" is "0.9". What is left of a build
// string after that can name a host's local patches, so it is never sent.
func MajorMinor(v string) string { return versionParts(v, 2) }

// Major cuts a version to its first numeric part: "550.54.15" is "550".
func Major(v string) string { return versionParts(v, 1) }

func versionParts(v string, n int) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	var parts []string
	for _, p := range strings.SplitN(v, ".", n+1) {
		digits := p
		if i := strings.IndexFunc(p, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			digits = p[:i]
		}
		if digits == "" {
			break
		}
		parts = append(parts, digits)
		if len(parts) == n || len(digits) != len(p) {
			break
		}
	}
	return strings.Join(parts, ".")
}
