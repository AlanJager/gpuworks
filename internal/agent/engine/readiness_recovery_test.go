package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Strata 0.1.38 (09817da, serve/server.py): /health is HTTP200 even
// while its child process is dead/loading; loaded becomes true only at READY.
// A crash lists no models; intentional idle unload lists an unloaded model.
func TestStrataChildRecoveryKeepsContainerAndReportsReadiness(t *testing.T) {
	var loaded atomic.Bool
	loaded.Store(true)
	var loads atomic.Int32
	release := make(chan struct{})
	disconnected := make(chan struct{})
	var once sync.Once
	resume := func() { once.Do(func() { close(release) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprintf(w, `{"status":"ok","service":"strata","loaded":%v,"api_key":false}`, loaded.Load())
		case "/v1/models":
			if loaded.Load() {
				fmt.Fprint(w, `{"object":"list","data":[{"id":"model"}]}`)
			} else {
				fmt.Fprint(w, `{"object":"list","data":[]}`)
			}
		case "/load":
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Origin") != "" {
				t.Errorf("invalid load request: %s %v", r.Method, r.Header)
				w.WriteHeader(415)
				return
			}
			io.Copy(io.Discard, r.Body)
			loads.Add(1)
			select {
			case <-release:
			case <-r.Context().Done():
				close(disconnected)
				<-release // Strata's loader continues despite HTTP disconnect.
			}
			loaded.Store(true)
			fmt.Fprint(w, `{"status":"loaded"}`)
		}
	}))
	defer server.Close()
	defer resume()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("child-recovery", serverPort(t, server))
	spec.Engine = engines.Callable
	serve(t, e, log, spec)
	before := len(log.states(proto.InstStarting))
	loaded.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.checkLiveness(ctx)
	starting := log.waitFor(t, proto.InstStarting, before+1)
	if got := starting[len(starting)-1]; got.Phase != proto.PhaseHealthCheck {
		t.Fatalf("recovery not visible: %+v", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for loads.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if loads.Load() != 1 {
		t.Fatalf("load attempts=%d, want one", loads.Load())
	}
	for count := 0; count < 3; count++ {
		e.checkLiveness(ctx)
	}
	if loads.Load() != 1 {
		t.Fatal("liveness started duplicate loads")
	}
	if !driver.exists(containerName(spec.UUID)) {
		t.Fatal("live HTTP container was replaced during child recovery")
	}
	// The short HTTP probe must not bound a model load: the server keeps
	// loading after the client disconnects, and the agent still waits READY.
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("load request did not release its HTTP connection")
	}
	if len(log.states(proto.InstRunning)) != 1 {
		t.Fatal("recovery declared ready before loading completed")
	}
	resume()
	log.waitFor(t, proto.InstRunning, 2)
	waitWatched(t, e, spec.UUID)
}

func TestStrataReadinessPreservesIntentionalUnloadAndUnknownProbes(t *testing.T) {
	for _, tc := range []struct {
		name, health, models string
		status               int
	}{
		{"intentional unload", `{"service":"strata","loaded":false,"api_key":false}`, `{"object":"list","data":[{"id":"model","status":{"value":"unloaded"}}]}`, 200},
		{"authenticated", `{"service":"strata","loaded":false,"api_key":true}`, `{"object":"list","data":[]}`, 200},
		{"models unauthorized", `{"service":"strata","loaded":false,"api_key":false}`, `{"error":"unauthorized"}`, 401},
		{"models malformed", `{"service":"strata","loaded":false,"api_key":false}`, `bad JSON`, 200},
		{"other service", `{"service":"other","loaded":false}`, `{"object":"list","data":[]}`, 200},
		{"unknown readiness", `{"service":"strata"}`, `{"object":"list","data":[]}`, 200},
		{"type invalid auth", `{"service":"strata","loaded":false,"api_key":"unsupported"}`, `{"object":"list","data":[]}`, 200},
		{"type invalid loaded", `{"service":"strata","loaded":"false","api_key":false}`, `{"object":"list","data":[]}`, 200},
		{"partial JSON", `{"service":"strata","loaded":false,"api_key":`, `{"object":"list","data":[]}`, 200},
		{"trailing JSON", `{"service":"strata","loaded":false,"api_key":false} {}`, `{"object":"list","data":[]}`, 200},
		{"auth absent", `{"service":"strata","loaded":false}`, `{"object":"list","data":[]}`, 200},
		{"auth null", `{"service":"strata","loaded":false,"api_key":null}`, `{"object":"list","data":[]}`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probe atomic.Bool
			var loads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					if probe.Load() {
						fmt.Fprint(w, tc.health)
					} else {
						fmt.Fprint(w, `{"service":"strata","loaded":true,"api_key":false}`)
					}
				case "/v1/models":
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.models)
				case "/load":
					loads.Add(1)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			driver := newLivenessDriver()
			e, log := newLivenessEngine(t, driver)
			spec := runningSpec("preserve", serverPort(t, server))
			spec.Engine = engines.Callable
			serve(t, e, log, spec)
			before := len(log.states(proto.InstStarting))
			probe.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.checkLiveness(ctx)
			waitHealthProbes(t, e)
			if len(log.states(proto.InstStarting)) != before || loads.Load() != 0 {
				t.Fatalf("unsupported/intentional state triggered recovery: %+v loads=%d", log.snapshot(), loads.Load())
			}
			waitWatched(t, e, spec.UUID)
		})
	}
}

func TestStrataStartupDoesNotTreatHTTP200LoadedFalseAsReady(t *testing.T) {
	var loaded atomic.Bool
	checked := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprintf(w, `{"service":"strata","loaded":%v,"api_key":false}`, loaded.Load())
			select {
			case checked <- struct{}{}:
			default:
			}
		}
	}))
	defer server.Close()
	e, log := newLivenessEngine(t, newLivenessDriver())
	spec := runningSpec("not-ready", serverPort(t, server))
	spec.Engine = engines.Callable
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.SetDesiredFull(ctx, []proto.InstanceDesiredState{spec})
	select {
	case <-checked:
	case <-time.After(5 * time.Second):
		t.Fatal("readiness was not checked")
	}
	if len(log.states(proto.InstRunning)) != 0 {
		t.Fatal("HTTP200 loaded=false was marked ready")
	}
	loaded.Store(true)
	log.waitFor(t, proto.InstRunning, 1)
	waitWatched(t, e, spec.UUID)
}

func TestStrataRecoveryCancellationDoesNotResurrectRemovedInstance(t *testing.T) {
	var loaded atomic.Bool
	loaded.Store(true)
	loadStarted := make(chan struct{})
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprintf(w, `{"service":"strata","loaded":%v,"api_key":false}`, loaded.Load())
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[]}`)
		case "/load":
			close(loadStarted)
			select {
			case <-r.Context().Done():
			case <-stop:
			}
		}
	}))
	defer server.Close()
	defer close(stop)
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("cancel-reload", serverPort(t, server))
	spec.Engine = engines.Callable
	serve(t, e, log, spec)
	loaded.Store(false)
	e.checkLiveness(context.Background())
	select {
	case <-loadStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("load was not requested")
	}
	e.SetDesiredFull(context.Background(), nil)
	log.waitFor(t, proto.InstStopped, 1)
	waitIdle(t, e, spec.UUID)
	if len(log.states(proto.InstRunning)) != 1 || len(log.states(proto.InstFailed)) != 0 {
		t.Fatalf("canceled recovery reported stale state: %+v", log.snapshot())
	}
	if driver.exists(containerName(spec.UUID)) {
		t.Fatal("removed recovery container leaked")
	}
}

func TestStrataLoadFailureDoesNotDeclareReadyOrRepeatedlyLoad(t *testing.T) {
	var loaded atomic.Bool
	loaded.Store(true)
	var loads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprintf(w, `{"service":"strata","loaded":%v,"api_key":false}`, loaded.Load())
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[]}`)
		case "/load":
			loads.Add(1)
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"GPU is busy"}}`)
		}
	}))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("load-failure", serverPort(t, server))
	spec.Engine = engines.Callable
	serve(t, e, log, spec)
	loaded.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.checkLiveness(ctx)
	deadline := time.Now().Add(5 * time.Second)
	seenFailure := false
	for time.Now().Before(deadline) {
		for _, report := range log.states(proto.InstStarting) {
			if strings.Contains(report.Message, "HTTP 503") {
				seenFailure = true
			}
		}
		if seenFailure {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !seenFailure {
		t.Fatalf("load failure not visible: %+v", log.snapshot())
	}
	if len(log.states(proto.InstRunning)) != 1 || loads.Load() != 1 || !driver.exists(containerName(spec.UUID)) {
		t.Fatal("load failure invented readiness, retried, or destroyed live container")
	}
	loaded.Store(true)
	log.waitFor(t, proto.InstRunning, 2)
	waitWatched(t, e, spec.UUID)
}

func TestStrataAdoptsIntentionallyUnloadedContainerWithoutReload(t *testing.T) {
	var loads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"service":"strata","loaded":false,"api_key":false}`)
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"model","status":{"value":"unloaded"}}]}`)
		case "/load":
			loads.Add(1)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("adopt-unloaded", serverPort(t, server))
	spec.Engine = engines.Callable
	if _, err := driver.Run(context.Background(), runtime.WorkloadSpec{Name: containerName(spec.UUID)}); err != nil {
		t.Fatal(err)
	}
	serve(t, e, log, spec)
	e.checkLiveness(context.Background())
	if loads.Load() != 0 || !driver.exists(containerName(spec.UUID)) || len(log.states(proto.InstFailed)) != 0 {
		t.Fatalf("intentional unload not preserved: loads=%d reports=%+v", loads.Load(), log.snapshot())
	}
	driver.mu.Lock()
	runs := driver.runs
	driver.mu.Unlock()
	if runs != 1 {
		t.Fatalf("adoption recreated container %d times", runs)
	}
}

func TestStrataAdoptionRestartsAlreadyCrashedChildExactlyOnce(t *testing.T) {
	var loaded atomic.Bool
	var loads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprintf(w, `{"service":"strata","loaded":%v,"api_key":false}`, loaded.Load())
		case "/v1/models":
			fmt.Fprint(w, `{"object":"list","data":[]}`)
		case "/load":
			loads.Add(1)
			loaded.Store(true)
			fmt.Fprint(w, `{"status":"loaded"}`)
		}
	}))
	defer server.Close()
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("adopt-crashed", serverPort(t, server))
	spec.Engine = engines.Callable
	if _, err := driver.Run(context.Background(), runtime.WorkloadSpec{Name: containerName(spec.UUID)}); err != nil {
		t.Fatal(err)
	}
	serve(t, e, log, spec)
	if loads.Load() != 1 || len(log.states(proto.InstFailed)) != 0 {
		t.Fatalf("crashed child not recovered: loads=%d reports=%+v", loads.Load(), log.snapshot())
	}
	driver.mu.Lock()
	runs := driver.runs
	driver.mu.Unlock()
	if runs != 1 {
		t.Fatal("child recovery recreated container")
	}
}

func TestStrataReadinessInvalidHealthPathDoesNotPanicOrReload(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("invalid-health-url", healthyEngine(t))
	spec.Engine = engines.Callable
	serve(t, e, log, spec)
	e.mu.Lock()
	spec.HealthPath = "/%invalid"
	e.desired[spec.UUID] = spec
	e.mu.Unlock()
	defer func() {
		if caught := recover(); caught != nil {
			t.Errorf("invalid health path panicked: %v", caught)
		}
	}()
	before := len(log.states(proto.InstStarting))
	e.checkLiveness(context.Background())
	waitHealthProbes(t, e)
	if len(log.states(proto.InstStarting)) != before {
		t.Fatal("invalid probe triggered recovery")
	}
	waitWatched(t, e, spec.UUID)
}

func TestNonStrataStartupHealthKeepsHTTPStatusContract(t *testing.T) {
	for _, body := range []string{"ok", `{"service":"other","loaded":false}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			e, log := newLivenessEngine(t, newLivenessDriver())
			spec := runningSpec("generic-health", serverPort(t, server))
			spec.Engine = engines.Callable
			serve(t, e, log, spec)
			if len(log.states(proto.InstRunning)) != 1 {
				t.Fatal("non-Strata HTTP200 health contract changed")
			}
		})
	}
}

func TestStrataStartupInvalidHealthPathFailsWithoutPanic(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("startup-invalid-url", 12345)
	spec.Engine = engines.Callable
	spec.HealthPath = "/%invalid"
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	failed := log.waitFor(t, proto.InstFailed, 1)
	if !strings.Contains(failed[0].Message, "invalid engine health endpoint") {
		t.Fatalf("invalid endpoint not explained: %+v", failed)
	}
	if len(log.states(proto.InstRunning)) != 0 || driver.exists(containerName(spec.UUID)) {
		t.Fatal("invalid startup probe marked ready or leaked container")
	}
}
