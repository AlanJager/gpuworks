//go:build linux

package fleet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// routeLoadPlugins is how many plugins the route-load case installs, each
// with the same four routes.
const routeLoadPlugins = 50

// TestFleetPluginRouteLoad drives plugin-route traffic through the API's
// whole request path (canonical path, authentication, feature gate,
// public rate limit, header filtering, response filtering) against
// in-process plugins that answer at once, so what it measures is the
// server's own cost per request: CPU-seconds and resident memory, while
// public requests come from 50k distinct client addresses and one hot
// address is held to its rate. Every slice that touches the plugin request
// path runs it:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 20m ./internal/integrationtest/fleet/ -run '^TestFleetPluginRouteLoad$'
//
// GPUWORKS_FLEET_ROUTE_REQUESTS (default 300000) and
// GPUWORKS_FLEET_ROUTE_ADDRESSES (default 50000) scale it.
func TestFleetPluginRouteLoad(t *testing.T) {
	skipUnlessPerf(t)
	requests := EnvInt("GPUWORKS_FLEET_ROUTE_REQUESTS", 300_000)
	addresses := EnvInt("GPUWORKS_FLEET_ROUTE_ADDRESSES", 50_000)

	st, err := store.Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	admin, _ := st.VerifyUser("admin", "gpuworks-test-password-2026")
	session, _, err := st.CreateManagementSession(admin, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreatePlatformToken(admin.ID, "load", store.PlatformTokenWrite, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("route-load-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := featuregate.New([]featuregate.Feature{{Key: "acme.backup"}}, fullFeatures{})
	if err != nil {
		t.Fatal(err)
	}
	plugins := newLoadPlugins(routeLoadPlugins)
	proxies, _ := auth.NewProxyTrust("192.0.2.1", nil)
	h := api.Router(api.Deps{Store: st, AuthSigner: signer, Extensions: api.ExtensionDeps{Features: gate, Dynamic: plugins.ext()}},
		auth.SessionPolicy{Proxies: proxies})

	var status [600]atomic.Int64
	var hotLimited atomic.Int64
	send := func(i int) {
		plugin := fmt.Sprintf("acme_p%02d", i%routeLoadPlugins)
		var req *http.Request
		switch i % 10 {
		case 0, 1, 2, 3, 4: // public, from one of addresses clients
			req = httptest.NewRequest(http.MethodGet, "/api/v1/x/"+plugin+"/", nil)
			a := (i/10*5 + i%10) % addresses
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.%d.%d.%d", a>>16&255, a>>8&255, a&255))
		case 5, 6, 7: // an administrator's gated read, by session cookie
			req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/x/%s/jobs/%d", plugin, i), nil)
			req.AddCookie(&http.Cookie{Name: "gpuworks_session", Value: session})
		case 8: // an administrator's write, by platform token
			req = httptest.NewRequest(http.MethodPost, "/api/v1/x/"+plugin+"/jobs", strings.NewReader(`{"n":1}`))
			req.Header.Set("Authorization", "Bearer "+token)
		default: // the hot public address
			req = httptest.NewRequest(http.MethodGet, "/api/v1/x/"+plugin+"/", nil)
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		status[rec.Code].Add(1)
		if i%10 == 9 && rec.Code == http.StatusTooManyRequests {
			hotLimited.Add(1)
		}
	}

	runtime.GC()
	rssBefore, cpuBefore := processRSSMiB(), cpuSeconds()
	start := time.Now()
	var next atomic.Int64
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) * 4 {
		wg.Go(func() {
			for i := int(next.Add(1)); i <= requests; i = int(next.Add(1)) {
				send(i)
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	cpu := cpuSeconds() - cpuBefore
	runtime.GC()
	rssGrowth := processRSSMiB() - rssBefore

	perRequest := time.Duration(cpu / float64(requests) * float64(time.Second))
	t.Logf("route load: %d requests in %v (%.0f/s), CPU %.2fs (%v per request), RSS +%.0f MiB; 200=%d 429=%d (hot %d) other=%d",
		requests, elapsed.Round(time.Millisecond), float64(requests)/elapsed.Seconds(), cpu, perRequest, rssGrowth,
		status[200].Load(), status[429].Load(), hotLimited.Load(), int64(requests)-status[200].Load()-status[429].Load())
	if other := int64(requests) - status[200].Load() - status[429].Load(); other != 0 {
		t.Errorf("%d requests answered neither 200 nor 429", other)
	}
	// The hot address is held to its rate; the distinct addresses are not.
	if hot := int64(requests / 10); hotLimited.Load() < hot*9/10 || status[429].Load() > hotLimited.Load() {
		t.Errorf("rate limit: hot address limited %d of %d, %d limited in all", hotLimited.Load(), hot, status[429].Load())
	}
	// Budgets: a plugin route request costs the server well under a
	// millisecond of CPU, and 50k client addresses do not grow the server
	// with them (the limiter keeps 10k buckets).
	if perRequest > 400*time.Microsecond {
		t.Errorf("CPU per request %v, budget 400µs", perRequest)
	}
	if rssGrowth > 192 {
		t.Errorf("RSS grew %.0f MiB, budget 192 MiB", rssGrowth)
	}
	if n := api.PublicLimiterSize(h); n > 10_000 || (addresses > 10_000 && n < 10_000) {
		t.Errorf("the public limiter holds %d buckets, bound 10000", n)
	}
}

func cpuSeconds() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()).Seconds()
}

type fullFeatures struct{}

func (fullFeatures) Availability(featuregate.Key) featuregate.Availability {
	return featuregate.Availability{Access: featuregate.Full}
}

// loadPlugins are plugins with a public home page, an administrator's
// gated read and write, answered in process.
type loadPlugins struct {
	routes map[string]map[string]*api.DynamicRoute // plugin -> "METHOD shape"
	list   []api.DynamicRoute
}

func newLoadPlugins(n int) *loadPlugins {
	p := &loadPlugins{routes: map[string]map[string]*api.DynamicRoute{}}
	for i := range n {
		id := fmt.Sprintf("acme_p%02d", i)
		p.routes[id] = map[string]*api.DynamicRoute{}
		for _, r := range []api.DynamicRoute{
			{Method: "GET", Pattern: "/", Access: api.DynamicPublic},
			{Method: "GET", Pattern: "/jobs/{id}", Access: api.DynamicAdmin, Feature: "acme.backup"},
			{Method: "POST", Pattern: "/jobs", Access: api.DynamicAdmin, Feature: "acme.backup"},
			{Method: "GET", Pattern: "/jobs", Access: api.DynamicAdmin, Feature: "acme.backup"},
		} {
			r.Plugin, r.Version, r.Action, r.MaxBody, r.Timeout = id, "1.0.0", id+".x", 1<<20, 10*time.Second
			r.RatePerMinute, r.RateBurst = 600, 20
			route := r
			p.routes[id][r.Method+" "+shape(r.Pattern)] = &route
			p.list = append(p.list, route)
		}
	}
	return p
}

func shape(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if strings.HasPrefix(s, "{") || (i > 0 && s != "" && s[0] >= '0' && s[0] <= '9') {
			segs[i] = "*"
		}
	}
	return strings.Join(segs, "/")
}

func (p *loadPlugins) ext() *api.DynamicExtension {
	return &api.DynamicExtension{
		Match: func(method, path string) (*api.DynamicRoute, api.DynamicMatch) {
			id, rest, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
			if r := p.routes[id][method+" "+shape("/"+rest)]; r != nil {
				return r, api.DynamicFound
			}
			return nil, api.DynamicMiss
		},
		Routes: func() []api.DynamicRoute { return p.list },
		Forward: func(_ context.Context, _ *api.DynamicRoute, req *http.Request, _ api.DynamicCaller) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, req.Body)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"ok":true}`)), ContentLength: 11}, nil
		},
		DecodeError: func([]byte) (*messages.ProductError, error) { return nil, fmt.Errorf("not used") },
	}
}
