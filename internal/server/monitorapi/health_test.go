package monitorapi

import (
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/healthcheck"
)

// The wire health is the deployment's own setting first: off whatever the
// checker still holds, pending when on and not checked yet; then the
// checker's latest status, its recent checks and the mean of those
// answered in time.
func TestHealthOnTheWire(t *testing.T) {
	found := map[uint]healthcheck.Health{
		1: {Status: healthcheck.StatusFailed, Threshold: time.Minute, Recent: []healthcheck.Probe{
			{AtMS: 1, Status: healthcheck.StatusOK, LatencyMS: 100, HTTPStatus: 200, InstanceUUID: "i-1"},
			{AtMS: 2, Status: healthcheck.StatusTimeout, LatencyMS: 60000},
			{AtMS: 3, Status: healthcheck.StatusOK, LatencyMS: 300, HTTPStatus: 200, InstanceUUID: "i-2"},
			{AtMS: 4, Status: healthcheck.StatusFailed, LatencyMS: 20, HTTPStatus: 503, InstanceUUID: "i-2", Message: "busy"},
		}},
		2: {Status: healthcheck.StatusNoReplica},
	}
	s := New(Deps{Health: func(id uint) healthcheck.Health {
		if h, ok := found[id]; ok {
			return h
		}
		return healthcheck.Health{Status: healthcheck.StatusOff}
	}})
	wire := func(id uint, enabled bool) string {
		raw, err := json.Marshal(s.Health(id, enabled, 60))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	for _, tc := range []struct {
		name    string
		id      uint
		enabled bool
		want    string
	}{
		{"checked", 1, true, `{"status":"failed","threshold_sec":60,"last":{"at_ms":4,"status":"failed","latency_ms":20,"http_status":503,"instance_uuid":"i-2","message":"busy"},"recent_mean_ms":200,"recent_ok":2,"recent":[` +
			`{"at_ms":1,"status":"ok","latency_ms":100,"http_status":200,"instance_uuid":"i-1"},{"at_ms":2,"status":"timeout","latency_ms":60000},` +
			`{"at_ms":3,"status":"ok","latency_ms":300,"http_status":200,"instance_uuid":"i-2"},{"at_ms":4,"status":"failed","latency_ms":20,"http_status":503,"instance_uuid":"i-2","message":"busy"}]}`},
		{"turned off with results left", 1, false, `{"status":"off","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]}`},
		{"on, not checked yet", 3, true, `{"status":"pending","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]}`},
		{"nothing to check", 2, true, `{"status":"no_replica","threshold_sec":60,"last":null,"recent_mean_ms":null,"recent_ok":0,"recent":[]}`},
	} {
		if got := wire(tc.id, tc.enabled); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
	if got := New(Deps{}).Health(1, true, 30); got.Status != "pending" || got.ThresholdSec != 30 {
		t.Errorf("no checker wired: %+v", got)
	}
}
