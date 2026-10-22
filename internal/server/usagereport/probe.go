package usagereport

import (
	"context"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Before a report is sent every endpoint is asked once, at the same time,
// with a GET that carries no report (SLS answers 400, the collector 405; a
// GET without parameters writes nothing), and the report goes to the one
// that answered first. The configured order breaks near ties, so the primary
// keeps the traffic unless another endpoint is clearly faster.
const (
	probeTimeout = 5 * time.Second
	// probeMargin is how much faster a later endpoint must answer to be
	// tried before an earlier one.
	probeMargin = 50 * time.Millisecond
)

// unreachable marks an endpoint that did not answer its probe.
const unreachable = -1

// probe measures how long each endpoint takes to answer, in milliseconds
// (unreachable when it did not answer within probeTimeout), and returns the
// endpoints in the order to try them.
func (s *Sender) probe(ctx context.Context, endpoints []Endpoint) ([]Endpoint, []int) {
	latency := make([]time.Duration, len(endpoints))
	var wg sync.WaitGroup
	for i, ep := range endpoints {
		wg.Add(1)
		go func() {
			defer wg.Done()
			latency[i] = s.probeOne(ctx, ep)
		}()
	}
	wg.Wait()
	// Each later endpoint pays probeMargin per place, so it goes first only
	// when it answered that much sooner; one that did not answer goes last.
	order := make([]int, len(endpoints))
	for i := range order {
		order[i] = i
	}
	key := func(i int) time.Duration {
		if latency[i] < 0 {
			return time.Duration(1<<62) + time.Duration(i)
		}
		return latency[i] + time.Duration(i)*probeMargin
	}
	sort.SliceStable(order, func(a, b int) bool { return key(order[a]) < key(order[b]) })
	sorted := make([]Endpoint, len(endpoints))
	ms := make([]int, len(endpoints))
	for i, j := range order {
		sorted[i] = endpoints[j]
	}
	for i, l := range latency {
		ms[i] = unreachable
		if l >= 0 {
			ms[i] = int(l.Milliseconds())
		}
	}
	return sorted, ms
}

func (s *Sender) probeOne(ctx context.Context, ep Endpoint) time.Duration {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.URL, nil)
	if err != nil {
		return -1
	}
	start := time.Now()
	resp, err := s.client().Do(req)
	if err != nil {
		return -1
	}
	elapsed := time.Since(start)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	// Any HTTP answer means the endpoint is there; a 5xx means it is not
	// taking reports right now.
	if resp.StatusCode >= 500 {
		return -1
	}
	return elapsed
}
