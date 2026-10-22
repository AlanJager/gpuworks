// Package usagereport builds, bounds and sends the daily report of the user
// experience improvement program: what an installation runs, what it is used
// for, which tasks it ran and where they failed, as enumerated values, counts
// and buckets only (docs/reference/usage-report.md lists every field).
//
// It is a leaf: the server assembles a Report from the store and its
// in-process counters, this package only shapes and ships it.
package usagereport

import (
	"encoding/json"
	"sort"
	"strings"
)

// Count is one value of a distribution and how many have it.
type Count struct {
	Name string
	N    int
}

// Counts is a distribution. On the wire it is an object of name to count, so
// the collector can read one value without unpacking a list.
type Counts []Count

// MarshalJSON writes the distribution as {"name": n, ...}.
func (c Counts) MarshalJSON() ([]byte, error) {
	m := make(map[string]int, len(c))
	for _, x := range c {
		m[x.Name] += x.N
	}
	return json.Marshal(m)
}

// UnmarshalJSON reads what MarshalJSON writes, largest first.
func (c *Counts) UnmarshalJSON(raw []byte) error {
	var m map[string]int
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	out := make(Counts, 0, len(m))
	for name, n := range m {
		out = append(out, Count{Name: name, N: n})
	}
	sortCounts(out)
	*c = out
	return nil
}

// Bounds that keep every distribution short however varied the fleet is: the
// biggest groups are kept, the rest fold into "other".
const (
	MaxGroups = 16
	MaxName   = 64
	Other     = "other"
	Unknown   = "unknown"
)

// Bound keeps the largest groups (at most groups of them, the last being
// "other" when anything was folded), names trimmed and cut to MaxName bytes,
// an empty name counted as "unknown". Zero and negative counts are dropped.
// The input is not modified.
func Bound(in []Count, groups int) []Count {
	merged := map[string]int{}
	for _, c := range in {
		if c.N <= 0 {
			continue
		}
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = Unknown
		}
		if len(name) > MaxName {
			name = strings.ToValidUTF8(name[:MaxName], "")
		}
		merged[name] += c.N
	}
	out := make([]Count, 0, len(merged))
	for name, n := range merged {
		out = append(out, Count{Name: name, N: n})
	}
	sortCounts(out)
	if groups < 1 {
		groups = 1
	}
	if len(out) <= groups {
		return out
	}
	rest := 0
	for _, c := range out[groups-1:] {
		rest += c.N
	}
	return append(out[:groups-1], Count{Name: Other, N: rest})
}

func sortCounts(out []Count) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Name < out[j].Name
	})
}
