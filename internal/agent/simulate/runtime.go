package simulate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/hostsim"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Runtime is the simulated container runtime. Starting an instance starts no
// container: the experience environment's engine host is asked to answer for
// the instance's model on its port. Every simulated node runs on one host
// and the agent reaches an engine at 127.0.0.1:<port>, so one process must
// own each port and answer for every model on it; that is the engine host.
type Runtime struct {
	spec  Spec
	cards *cards
	http  *http.Client

	mu      sync.Mutex
	seq     int
	byName  map[string]string // workload name -> container id
	running map[string]bool   // container id -> running
	exited  map[string]bool   // container id -> exited (one-shot probes)
	claimed map[string]bool   // container id -> holds an engine port
}

// New returns spec's runtime and prober, which share what each instance
// holds so card metrics follow the instances.
func New(spec Spec) (*Runtime, *Prober) {
	c := newCards()
	r := &Runtime{spec: spec, cards: c, http: &http.Client{Timeout: 10 * time.Second},
		byName: map[string]string{}, running: map[string]bool{}, exited: map[string]bool{}, claimed: map[string]bool{}}
	return r, NewProber(spec, c)
}

// Name is docker: the node presents the runtime a real server would have.
func (r *Runtime) Name() string                         { return "docker" }
func (r *Runtime) Detect(context.Context) (bool, error) { return true, nil }

// Images are always present: nothing is pulled on a simulated node.
func (r *Runtime) ImageExists(context.Context, string) (bool, error)     { return true, nil }
func (r *Runtime) PullImage(context.Context, string, func(string)) error { return nil }
func (r *Runtime) LoadImage(context.Context, string, io.Reader) error    { return nil }
func (r *Runtime) Prepare(context.Context, runtime.WorkloadSpec) error   { return nil }
func (r *Runtime) Exec(context.Context, string, []string) ([]byte, error) {
	return nil, hostsim.ErrRefused
}
func (r *Runtime) BindHostPort(int) (io.Closer, error) { return io.NopCloser(nil), nil }
func (r *Runtime) InspectImage(context.Context, string) (runtime.ImageInspection, error) {
	return runtime.ImageInspection{}, nil
}

func (r *Runtime) Run(ctx context.Context, s runtime.WorkloadSpec) (string, error) {
	r.mu.Lock()
	r.seq++
	id := fmt.Sprintf("sim-%d", r.seq)
	r.mu.Unlock()
	// The dGPU probe is a one-shot container that exits cleanly, as it does
	// on a supported card.
	if s.Image == assets.PartitionProbeImage {
		r.mu.Lock()
		r.byName[s.Name], r.exited[id] = id, true
		r.mu.Unlock()
		return id, nil
	}
	// The first port is the engine's API port, except on a headless rank
	// (a multi-node follower), which serves no API and only runs.
	if len(s.Ports) > 0 && !containsArg(s.Cmd, "--headless") {
		launch := append(append([]string(nil), s.Entrypoint...), s.Cmd...)
		if err := r.call(ctx, "/claim", map[string]any{"node": r.spec.Hostname, "container": id, "port": s.Ports[0], "model": servedModel(launch, s.Name)}); err != nil {
			return "", fmt.Errorf("start the simulated engine: %w", err)
		}
		r.mu.Lock()
		r.claimed[id] = true
		r.mu.Unlock()
		r.cards.claim(id, s.Ports[0])
	}
	r.mu.Lock()
	r.byName[s.Name], r.running[id] = id, true
	r.mu.Unlock()
	r.cards.hold(id, s.GPUIndexes, r.share(s))
	return id, nil
}

func (r *Runtime) Stop(ctx context.Context, id string) error {
	r.mu.Lock()
	if cid, ok := r.byName[id]; ok {
		id = cid
	}
	if r.running[id] {
		r.running[id], r.exited[id] = false, true
	}
	claimed := r.claimed[id]
	r.mu.Unlock()
	r.cards.release(id)
	if !claimed {
		return nil
	}
	// The claim is forgotten only once the engine host let it go, so a
	// failed release is retried by the next Stop or Remove.
	if err := r.call(ctx, "/release", map[string]any{"node": r.spec.Hostname, "container": id}); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.claimed, id)
	r.mu.Unlock()
	return nil
}

// ReleaseAll lets go of every engine the node held before this process: a
// restarted agent starts with no containers, and the engines its previous
// process claimed would otherwise keep answering.
func (r *Runtime) ReleaseAll(ctx context.Context) error {
	return r.call(ctx, "/release", map[string]any{"node": r.spec.Hostname})
}

func (r *Runtime) Remove(ctx context.Context, id string) error {
	r.mu.Lock()
	if cid, ok := r.byName[id]; ok {
		id = cid
	}
	r.mu.Unlock()
	if err := r.Stop(ctx, id); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, cid := range r.byName {
		if cid == id {
			delete(r.byName, name)
		}
	}
	delete(r.running, id)
	delete(r.exited, id)
	return nil
}

func (r *Runtime) Status(_ context.Context, id string) (runtime.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cid, ok := r.byName[id]; ok {
		id = cid
	}
	switch {
	case r.running[id]:
		return runtime.Status{Exists: true, Running: true}, nil
	case r.exited[id]:
		return runtime.Status{Exists: true}, nil
	}
	return runtime.Status{}, nil
}

func (r *Runtime) ListContainers(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (r *Runtime) RunningContainers(context.Context) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for name, id := range r.byName {
		if r.running[id] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Logs are what a vLLM server prints: its startup, then a throughput line
// every ten seconds while followed.
func (r *Runtime) Logs(ctx context.Context, _ string, follow bool, _ int) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		stamp := func() string { return time.Now().Format("01-02 15:04:05") }
		for _, line := range []string{
			"Starting vLLM API server on http://0.0.0.0:8000",
			"Loading model weights took 6.12 GiB and 8.4 seconds",
			"# GPU blocks: 18432, # CPU blocks: 2048",
			"Graph capturing finished in 4 secs",
			"Application startup complete.",
		} {
			if _, err := fmt.Fprintf(pw, "INFO %s [api_server.py:1024] %s\n", stamp(), line); err != nil {
				return
			}
		}
		for follow {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			if _, err := fmt.Fprintf(pw, "INFO %s [metrics.py:455] Avg prompt throughput: %.1f tokens/s, Avg generation throughput: %.1f tokens/s, Running: %d reqs, Pending: 0 reqs, GPU KV cache usage: %.1f%%.\n",
				stamp(), 40+rand.Float64()*160, 20+rand.Float64()*90, rand.IntN(4), 8+rand.Float64()*20); err != nil {
				return
			}
		}
	}()
	return pr, nil
}

// share is how much of each card an instance holds: its dGPU memory limit
// over the card's memory, or all of it.
func (r *Runtime) share(s runtime.WorkloadSpec) float64 {
	limit, err := strconv.ParseInt(s.Env[accelerator.DGPUEnvMemoryLimitPrefix+"0"], 10, 64)
	total := r.spec.VRAMMiB << 20
	if err != nil || limit <= 0 || total <= 0 {
		return 1
	}
	return min(1, float64(limit)/float64(total))
}

// call posts body to the engine host.
func (r *Runtime) call(ctx context.Context, path string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.spec.Engines, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+r.spec.EnginesToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("engine host: %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// servedModel is the launch's --served-model-name, as an engine reads it.
func servedModel(argv []string, fallback string) string {
	for i, a := range argv {
		if a == "--served-model-name" && i+1 < len(argv) {
			return argv[i+1]
		}
		if v, ok := strings.CutPrefix(a, "--served-model-name="); ok {
			return v
		}
	}
	return fallback
}

func containsArg(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

// EnginePortListeners report every port of r outside the node's own slice
// of it as taken (hostsim.Host.Listeners): the simulated nodes share one host,
// and with a slice each the console never gives two of them the same port.
func (p *Prober) EnginePortListeners(r proto.PortRange) []int {
	if p.spec.PortSlots <= 1 {
		return nil
	}
	size := (r.End - r.Start + 1) / p.spec.PortSlots
	own := r.Start + p.spec.PortSlot*size
	out := make([]int, 0, r.End-r.Start+1-size)
	for port := r.Start; port <= r.End; port++ {
		if port < own || port >= own+size {
			out = append(out, port)
		}
	}
	return out
}
