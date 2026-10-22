//go:build docker

package l3

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// monitorSeries/monitorSignal mirror the /api/v1/monitor wire contract's
// Series/Signal shapes (internal/server/api/dto/monitor.go) without
// importing that package, matching this package's other fixtures
// (v4DockerFixture and friends) which decode the HTTP wire directly rather
// than reaching into server-internal types from a real-container test.
type monitorSeries struct {
	Labels map[string]string `json:"labels"`
	Points [][2]float64      `json:"points"`
}

type monitorSignal struct {
	Key       string         `json:"key"`
	Collected bool           `json:"collected"`
	Detail    map[string]any `json:"detail"`
}

type monitorOverview struct {
	Signals []monitorSignal `json:"signals"`
}

// chatCompletion posts one /v1/chat/completions request through the proxy
// with the given API key, returning its status and body.
func chatCompletion(t *testing.T, baseURL, apiKey, model string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// setFakeEngineStatusOverride flips a running fake-engine instance's forced
// /v1/chat/completions status directly (test/fake-engine/handler/handler.go's
// /debug/status-override) - code=0 clears it, restoring normal responses.
func setFakeEngineStatusOverride(t *testing.T, apiPort, code int) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/debug/status-override?code=%d", apiPort, code)
	resp, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatalf("set status override to %d: %v", code, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set status override to %d: status=%d", code, resp.StatusCode)
	}
}

// getMonitorSeries fetches /api/v1/monitor/series through the management API
// client and decodes it.
func getMonitorSeries(t *testing.T, api interface {
	Get(string) (*http.Response, error)
}, path string) []monitorSeries {
	t.Helper()
	resp, err := api.Get(path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d body=%s", path, resp.StatusCode, data)
	}
	var series []monitorSeries
	if err := apiwire.UnmarshalList(data, &series); err != nil {
		t.Fatalf("decode %s: %v (%s)", path, err, data)
	}
	return series
}

// TestProxyAccountingClassifiesStatusesAndSleepRejections drives real
// traffic through the real /v1 proxy against a real, containerized
// fake-engine instance and proves the full accounting pipeline end to end
// (plan artifacts/archive/2026-09-19-monitor-center/plan.md §8: "L3：验证 proxy 计数
// 与睡眠 503 归类"): a 2xx success, a 429 and a 5xx forced via the engine's
// own status-override knob, and an asleep_503 rejection once the deployment
// is put to sleep, all end up correctly classified in
// gpuworks_proxy_requests_total's classes and in the cluster overview's
// error_rate signal detail, and the in-flight gauge returns to 0 once every
// request has completed.
func TestProxyAccountingClassifiesStatusesAndSleepRejections(t *testing.T) {
	fixture := newV4DockerFixture(t, "proxy-accounting")
	depID, instance := fixture.createDeployment(t, map[string]any{
		"image": fixture.imageRef, "served_name": "l3-proxy-accounting", "sleepable": true,
	})
	depKey := strconv.FormatUint(uint64(depID), 10)

	apiKey, err := fixture.env.Store().CreateAPIKey("l3-proxy-accounting", 0)
	if err != nil {
		t.Fatal(err)
	}
	baseURL := fixture.env.URL()
	model := "l3-proxy-accounting"

	// 2xx: a normal chat completion succeeds.
	if status, body := chatCompletion(t, baseURL, apiKey, model); status != http.StatusOK {
		t.Fatalf("expected a 2xx from a healthy instance, got status=%d body=%s", status, body)
	}

	// 429/5xx need to show up as a *positive* error_rate.detail percentage
	// below, which needs an actual rate - and Accountant.flush only ever
	// writes a fresh sample for a class when its deployment is dirty
	// (accounting.go's snapshotAndClearDirty), so a single burst of requests
	// gives every class exactly one point ever and Rate() (which needs two
	// points to produce anything) reads that as zero forever. Driving each
	// class continuously for longer than one flush interval (10s) guarantees
	// at least two flushes see it grow.
	driveClassFor := func(d time.Duration, code int, wantStatus int) {
		setFakeEngineStatusOverride(t, instance.APIPort, code)
		defer setFakeEngineStatusOverride(t, instance.APIPort, 0)
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if status, body := chatCompletion(t, baseURL, apiKey, model); status != wantStatus {
				t.Fatalf("expected status %d with override code=%d, got status=%d body=%s", wantStatus, code, status, body)
			}
			time.Sleep(4 * time.Second)
		}
	}
	driveClassFor(25*time.Second, http.StatusTooManyRequests, http.StatusTooManyRequests)
	driveClassFor(25*time.Second, http.StatusInternalServerError, http.StatusInternalServerError)

	// asleep_503: put the deployment to sleep, then try to serve it. The
	// proxy must reject before ever dispatching to the engine, purely from
	// the store's own SleepLevel bookkeeping (proxy/proxy.go's
	// selectDeploymentInstance) - so this classifies as asleep_503
	// regardless of the engine's own status-override state.
	sleepResp, err := fixture.api.PostJSON("/api/v1/deployments/"+strconv.FormatUint(uint64(depID), 10)+"/sleep?level=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	sleepBody, _ := io.ReadAll(sleepResp.Body)
	sleepResp.Body.Close()
	if sleepResp.StatusCode != http.StatusOK {
		t.Fatalf("sleep action status=%d body=%s", sleepResp.StatusCode, sleepBody)
	}
	fixture.env.Eventually(t, 60*time.Second, func() bool {
		instances, err := fixture.env.Store().InstancesForDeployment(depID)
		return err == nil && len(instances) == 1 && instances[0].SleepLevel == 1
	})
	// The store's sleep bookkeeping and the proxy's view of it converge
	// asynchronously: the proxy dispatches from a route snapshot that is
	// refreshed in the background off instance events, so a request racing
	// the rebuild can still be dispatched to the just-slept instance (its
	// engine keeps answering at sleep level 1) and return 200. Wait for the
	// proxy to converge on the rejection first; the sustained window below
	// then proves the rejection is STABLE, not merely that it eventually
	// happened. Catch-up polls land in the 2xx accounting class, which the
	// later assertions only require to exist, not to balance.
	fixture.env.Eventually(t, 30*time.Second, func() bool {
		status, body := chatCompletion(t, baseURL, apiKey, model)
		return status == http.StatusServiceUnavailable && strings.Contains(body, "asleep")
	})
	// Same reasoning as the 429/5xx classes above: keep asking for longer
	// than one flush interval so asleep_503 also gets a positive rate.
	asleepDeadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(asleepDeadline) {
		if status, body := chatCompletion(t, baseURL, apiKey, model); status != http.StatusServiceUnavailable || !strings.Contains(body, "asleep") {
			t.Fatalf("expected a 503 'asleep' rejection once the instance is asleep, got status=%d body=%s", status, body)
		}
		time.Sleep(4 * time.Second)
	}

	// Poll rather than sleeping a fixed amount so this isn't flaky under a
	// slow CI runner.
	seriesPath := "/api/v1/monitor/series?metric=gpuworks_proxy_requests_total&by=deployment&range=15m"
	fixture.env.Eventually(t, 30*time.Second, func() bool {
		for _, s := range getMonitorSeries(t, fixture.api, seriesPath) {
			if s.Labels["deployment"] == depKey && len(s.Points) > 0 {
				return true
			}
		}
		return false
	})
	series := getMonitorSeries(t, fixture.api, seriesPath)
	found := false
	for _, s := range series {
		if s.Labels["deployment"] != depKey {
			continue
		}
		found = true
		last := s.Points[len(s.Points)-1]
		if last[1] <= 0 {
			t.Fatalf("gpuworks_proxy_requests_total for deployment %s = %v, want > 0 after 4 requests across every class", depKey, last[1])
		}
	}
	if !found {
		t.Fatalf("no gpuworks_proxy_requests_total series for deployment %s in %+v", depKey, series)
	}

	getOverview := func() monitorOverview {
		resp, err := fixture.api.Get("/api/v1/monitor/overview")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET overview status=%d body=%s", resp.StatusCode, data)
		}
		var overview monitorOverview
		if err := json.Unmarshal(data, &overview); err != nil {
			t.Fatalf("decode overview: %v (%s)", err, data)
		}
		return overview
	}
	errRateReady := func(overview monitorOverview) (*monitorSignal, bool) {
		for i := range overview.Signals {
			if overview.Signals[i].Key != "error_rate" {
				continue
			}
			sig := &overview.Signals[i]
			if !sig.Collected {
				return sig, false
			}
			for _, class := range []string{"5xx", "429", "asleep_503"} {
				pct, isNum := sig.Detail[class].(float64)
				if !isNum || pct <= 0 {
					return sig, false
				}
			}
			return sig, true
		}
		return nil, false
	}
	var lastOverview monitorOverview
	fixture.env.Eventually(t, 30*time.Second, func() bool {
		lastOverview = getOverview()
		_, ready := errRateReady(lastOverview)
		return ready
	})
	if _, ready := errRateReady(lastOverview); !ready {
		t.Fatalf("overview error_rate never showed a positive percent for every class: %+v", lastOverview.Signals)
	}

	// The in-flight gauge must settle back to 0 once every request above has
	// completed (none of them stream, so this should already be true, but
	// asserting it end to end through tsdb is the point).
	inflightPath := "/api/v1/monitor/series?metric=gpuworks_proxy_requests_inflight&by=deployment&range=15m"
	inflight := getMonitorSeries(t, fixture.api, inflightPath)
	for _, s := range inflight {
		if s.Labels["deployment"] != depKey || len(s.Points) == 0 {
			continue
		}
		if last := s.Points[len(s.Points)-1][1]; last != 0 {
			t.Fatalf("gpuworks_proxy_requests_inflight for deployment %s = %v after every request completed, want 0", depKey, last)
		}
	}

	fixture.deleteDeployment(t, depID)
}
