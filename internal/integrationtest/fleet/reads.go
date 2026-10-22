package fleet

import (
	"fmt"
	"sync"
	"time"
)

// ListReadBudget is the capacity acceptance limit for an operator list poll,
// including response-body delivery. Slow and failed reads both fail the run.
const ListReadBudget = 10 * time.Second

func readProblems(path string, latency *Latencies, failures []string, minimum int) []string {
	var problems []string
	if latency.Count() < minimum {
		problems = append(problems, fmt.Sprintf("%s: only %d successful samples, need %d", path, latency.Count(), minimum))
	}
	if len(failures) > 0 {
		problems = append(problems, fmt.Sprintf("%s: %d read(s) failed: %v", path, len(failures), failures))
	}
	if worst := latency.Percentile(1); worst > ListReadBudget {
		problems = append(problems, fmt.Sprintf("%s: maximum %s exceeded %s", path, worst, ListReadBudget))
	}
	return problems
}

// ListReadProbe observes each list independently while nodes enroll/connect.
// A slow route cannot suppress observations of other routes. Each phase must
// supply real samples; successful final node count alone is not acceptance.
type ListReadProbe struct {
	phase     string
	minimum   int
	stop      chan struct{}
	done      chan struct{}
	sampled   chan struct{}
	stopOnce  sync.Once
	paths     []string
	latencies []*Latencies
	failures  [][]string
}

// StartListReadProbe starts one request on every route before returning. A
// short onboarding phase can therefore stop immediately without inventing
// post-onboarding samples. Stable phases can request more observations.
func StartListReadProbe(c *Client, phase string, minimum int) *ListReadProbe {
	if minimum < 1 {
		minimum = 1
	}
	p := &ListReadProbe{phase: phase, minimum: minimum, stop: make(chan struct{}), done: make(chan struct{}), sampled: make(chan struct{}), paths: []string{"/api/v1/nodes", "/api/v1/nodes?page=1&page_size=30", "/api/v1/deployments", "/api/v1/instances"}}
	p.latencies = make([]*Latencies, len(p.paths))
	p.failures = make([][]string, len(p.paths))
	var wg, started, sampled sync.WaitGroup
	for i, path := range p.paths {
		p.latencies[i] = &Latencies{}
		wg.Add(1)
		started.Add(1)
		sampled.Add(1)
		go func() {
			defer wg.Done()
			reached := false
			defer func() {
				if !reached {
					sampled.Done()
				}
			}()
			for attempts := 1; ; attempts++ {
				took, err := c.discard(path, func() {
					if attempts == 1 {
						started.Done()
					}
				})
				if err != nil {
					p.failures[i] = append(p.failures[i], err.Error())
				} else {
					p.latencies[i].Add(took)
				}
				if attempts == p.minimum {
					reached = true
					sampled.Done()
				}
				select {
				case <-p.stop:
					return
				case <-time.After(250 * time.Millisecond):
				}
				select {
				case <-p.stop:
					return
				default:
				}
			}
		}()
	}
	go func() { wg.Wait(); close(p.done) }()
	go func() { sampled.Wait(); close(p.sampled) }()
	started.Wait()
	return p
}

// WaitSamples bounds a stable observation window by the requested number of
// samples and the per-request budget. Failed attempts complete the window but
// still fail Stop's verdict; there is no retry-until-green behavior.
func (p *ListReadProbe) WaitSamples() error {
	timeout := time.NewTimer(time.Duration(p.minimum)*(ListReadBudget+250*time.Millisecond) + time.Second)
	defer timeout.Stop()
	select {
	case <-p.sampled:
		return nil
	case <-timeout.C:
		return fmt.Errorf("%s: sampling did not finish within its bounded window", p.phase)
	}
}

// Stop waits for in-flight requests (bounded by ListReadBudget), then returns
// both printable evidence and the verdict. Repeated calls return the same evidence.
func (p *ListReadProbe) Stop() (lines, problems []string) {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
	for i, path := range p.paths {
		l := p.latencies[i]
		name := p.phase + " " + path
		lines = append(lines, fmt.Sprintf("%s: n=%d failed=%d p95=%s max=%s", name, l.Count(), len(p.failures[i]), l.Percentile(.95).Round(time.Millisecond), l.Percentile(1).Round(time.Millisecond)))
		problems = append(problems, readProblems(name, l, p.failures[i], p.minimum)...)
	}
	return
}
