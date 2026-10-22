package registryprobe

import (
	"context"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Report describes one Rank call: the distinct hosts of the ranked runs in
// the order their candidates now take. Empty when no run was long enough to
// probe.
type Report struct {
	Hosts []HostResult
}

// String is a one-line summary such as "a.io (35 ms), b.io (unreachable)".
func (r Report) String() string {
	parts := make([]string, 0, len(r.Hosts))
	for _, h := range r.Hosts {
		switch {
		case h.Cooled:
			parts = append(parts, h.Host+" (cooling down)")
		case !h.Reachable:
			parts = append(parts, h.Host+" (unreachable)")
		default:
			parts = append(parts, h.Host+" ("+strconv.FormatInt(h.LatencyMS, 10)+" ms)")
		}
	}
	return strings.Join(parts, ", ")
}

// Rank reorders each maximal run of two or more adjacent Rank candidates by
// (cooled last, reachable first, lowest latency first) and leaves every other
// candidate where it is. A run of one is not worth a probe. Candidates are
// never removed: a cooled or unreachable host still gets its turn, last.
func (p *Prober) Rank(ctx context.Context, cands []proto.ImagePullCandidate, route Route) ([]proto.ImagePullCandidate, Report) {
	out := make([]proto.ImagePullCandidate, len(cands))
	copy(out, cands)
	var report Report
	for start := 0; start < len(out); {
		if !out[start].Rank {
			start++
			continue
		}
		end := start
		for end < len(out) && out[end].Rank {
			end++
		}
		if end-start >= 2 {
			report.Hosts = append(report.Hosts, p.rankRun(ctx, out[start:end], route)...)
		}
		start = end
	}
	return out, report
}

// rankRun sorts one run in place and returns its distinct hosts in the new
// order.
func (p *Prober) rankRun(ctx context.Context, run []proto.ImagePullCandidate, route Route) []HostResult {
	hosts := make([]string, 0, len(run))
	seen := make(map[string]bool, len(run))
	for _, c := range run {
		host := HostOf(c.Ref)
		if !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	results := p.Probe(ctx, hosts, route)
	// Insertion sort is stable, and a run is a handful of candidates.
	for i := 1; i < len(run); i++ {
		for j := i; j > 0 && better(results[HostOf(run[j].Ref)], results[HostOf(run[j-1].Ref)]); j-- {
			run[j], run[j-1] = run[j-1], run[j]
		}
	}
	var ordered []HostResult
	listed := map[string]bool{}
	for _, c := range run {
		host := HostOf(c.Ref)
		if !listed[host] {
			listed[host] = true
			ordered = append(ordered, results[host])
		}
	}
	return ordered
}

func better(a, b HostResult) bool {
	if a.Cooled != b.Cooled {
		return !a.Cooled
	}
	if a.Reachable != b.Reachable {
		return a.Reachable
	}
	return a.Reachable && a.LatencyMS < b.LatencyMS
}
