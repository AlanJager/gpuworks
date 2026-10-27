package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// A probe takes a client request's path to a replica and back, but leaves
// no trace in the deployment's figures: no counters, no request record, no
// last request time. A client request through the same proxy does.
func TestAProbeIsAnsweredButNotAccounted(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.URL.Path+" "+string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"1"}}],"usage":{"prompt_tokens":9,"completion_tokens":1}}`)
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	var records []uxrecord.Request
	p.SetRequestSink(func(r uxrecord.Request) { records = append(records, r) })

	reply := p.Probe(context.Background(), depID, chatPath, map[string]any{"messages": []map[string]any{{"role": "user", "content": "1"}}, "max_tokens": 1})
	if reply.NoReplica || reply.HTTPStatus != http.StatusOK || reply.InstanceUUID != "omni-instance" || reply.Message != "" {
		t.Fatalf("reply = %+v", reply)
	}
	if len(bodies) != 1 || bodies[0] != chatPath+` {"max_tokens":1,"messages":[{"content":"1","role":"user"}],"model":"omni"}` {
		t.Fatalf("the replica received %q", bodies)
	}
	if _, ok := acct.LastRequestAt(depID); ok {
		t.Error("a probe set the deployment's last request time")
	}
	if _, samples := acct.snapshotAndClearDirty(); len(samples) != 0 {
		t.Errorf("a probe was accounted: %+v", samples)
	}
	if len(records) != 0 {
		t.Errorf("a probe left a request record: %+v", records)
	}
	if live := p.LiveRequests(depID, 10); len(live.Requests) != 0 {
		t.Errorf("a probe is still in flight: %+v", live)
	}

	// The same proxy accounts a client's request.
	req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	p.Handler()(httptest.NewRecorder(), req)
	if _, ok := acct.LastRequestAt(depID); !ok {
		t.Error("a client request did not set the last request time")
	}
	if _, samples := acct.snapshotAndClearDirty(); len(samples) == 0 {
		t.Error("a client request was not accounted")
	}
	if len(records) != 1 {
		t.Errorf("records = %d", len(records))
	}
}

func TestAProbeReportsAnErrorReplyATimeoutAndNoReplica(t *testing.T) {
	slow := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "slow") {
			select {
			case <-slow:
			case <-r.Context().Done():
			}
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"`+strings.Repeat("engine unavailable. ", 30)+`"}}`)
	})
	p, acct, _, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	defer close(slow)

	reply := p.Probe(context.Background(), depID, chatPath, map[string]any{"input": "1"})
	if reply.HTTPStatus != http.StatusInternalServerError || reply.InstanceUUID != "omni-instance" || !strings.HasPrefix(reply.Message, `{"error":{"message":"engine unavailable.`) || len(reply.Message) > probeMessageMax+len("…") {
		t.Fatalf("error reply = %+v", reply)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	reply = p.Probe(ctx, depID, chatPath, map[string]any{"input": "slow"})
	if reply.Message != context.DeadlineExceeded.Error() || reply.NoReplica {
		t.Fatalf("timed out = %+v", reply)
	}
	if _, samples := acct.snapshotAndClearDirty(); len(samples) != 0 {
		t.Errorf("failed probes were accounted: %+v", samples)
	}

	// Asleep: nothing to send to.
	if err := p.store.UpdateInstanceStatus(proto.InstanceStatus{UUID: "omni-instance", State: proto.InstRunning, SleepLevel: 1, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
	if reply := p.Probe(context.Background(), depID, chatPath, map[string]any{"input": "1"}); !reply.NoReplica || reply.HTTPStatus != 0 {
		t.Fatalf("asleep = %+v", reply)
	}
	if reply := p.Probe(context.Background(), depID+100, chatPath, map[string]any{"input": "1"}); !reply.NoReplica {
		t.Fatalf("unknown deployment = %+v", reply)
	}
}
