package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func sampleValue(samples []tsdb.Sample, metric string, labels map[string]string) (float64, bool) {
	for _, sample := range samples {
		if sample.Metric != metric {
			continue
		}
		match := true
		for k, v := range labels {
			if sample.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return sample.Value, true
		}
	}
	return 0, false
}

// Real replies forwarded through the gateway reach the client byte for byte,
// and the deployment's series carry their engine-reported tokens and a first
// token time.
func TestGatewayCountsTokensAndFirstTokenOfForwardedReplies(t *testing.T) {
	stream := readFixture(t, "strata-stream.sse")
	plain := `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":8}}}`
	noUsage := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"stream":true`) && strings.Contains(string(body), "no-usage"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, noUsage)
		case strings.Contains(string(body), `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			// The opener first; the first token 30ms later, like an engine
			// that prefills before it decodes.
			events := strings.SplitAfter(stream, "\n\n")
			_, _ = io.WriteString(w, events[0])
			flusher.Flush()
			time.Sleep(30 * time.Millisecond)
			for _, event := range events[1:] {
				_, _ = io.WriteString(w, event)
				flusher.Flush()
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, plain)
		}
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()

	send := func(body string) string {
		req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		rec := httptest.NewRecorder()
		p.Handler()(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if got := send(`{"model":"omni","stream":true}`); got != stream {
		t.Fatalf("the client received %d bytes, the engine sent %d", len(got), len(stream))
	}
	if got := send(`{"model":"omni"}`); got != plain {
		t.Fatalf("plain reply altered: %s", got)
	}
	send(`{"model":"omni","stream":true,"user":"no-usage"}`)

	_, samples := acct.snapshotAndClearDirty()
	dep := map[string]string{"deployment": itoa(depID)}
	for metric, want := range map[string]float64{
		metricPromptTokens:     64 + 11,
		metricCompletionTokens: 80 + 2,
		metricCachedTokens:     59 + 8,
		metricUsageMissing:     1,
		metricTTFTCount:        2, // the two streams; the plain reply is not timed
		metricDurationCount:    3,
	} {
		if got, ok := sampleValue(samples, metric, dep); !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", metric, got, ok, want)
		}
	}
	// Buckets are cumulative and end in +Inf = count.
	if got, _ := sampleValue(samples, metricTTFTBucket, map[string]string{"deployment": itoa(depID), "le": "+Inf"}); got != 2 {
		t.Errorf("ttft +Inf bucket = %v", got)
	}
	if got, _ := sampleValue(samples, metricDurationBucket, map[string]string{"deployment": itoa(depID), "le": "+Inf"}); got != 3 {
		t.Errorf("duration +Inf bucket = %v", got)
	}
	if sum, _ := sampleValue(samples, metricTTFTSum, dep); sum < 30 {
		t.Errorf("ttft sum = %vms, the first token came after a 30ms prefill", sum)
	}
}

// A client that walks away mid-stream is not counted as a reply without
// usage: its usage never had the chance to arrive.
func TestGatewayDoesNotCountAnAbandonedStreamAsMissingUsage(t *testing.T) {
	release := make(chan struct{})
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(`{"model":"omni","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &firstWriteRecorder{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Handler()(client, req)
	}()
	// Leave once the first event has reached the client.
	select {
	case <-client.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("the first event never reached the client")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxied request did not end after the client left")
	}
	_, samples := acct.snapshotAndClearDirty()
	dep := map[string]string{"deployment": itoa(depID)}
	if got, ok := sampleValue(samples, metricUsageMissing, dep); ok && got != 0 {
		t.Fatalf("abandoned stream counted as missing usage: %v", got)
	}
	if got, _ := sampleValue(samples, metricTTFTCount, dep); got != 1 {
		t.Fatalf("its first token was still timed: ttft count = %v", got)
	}
}

// A client that leaves as soon as it has the reply's last event, before the
// stream's terminator, got the whole reply: its tokens count, and its record
// says it ended normally with its usage.
func TestGatewayCountsAReplyWhoseClientLeftAfterItsLastEvent(t *testing.T) {
	release := make(chan struct{})
	last := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":15,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":0}}}` + "\n\n"
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		_, _ = io.WriteString(w, last)
		w.(http.Flusher).Flush()
		// The terminator never comes before the client leaves.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxyFor(t, upstream, &store.Deployment{Engine: "callable"})
	defer closeUpstream()
	defer close(release)
	var records []uxrecord.Request
	var mu sync.Mutex
	p.SetRequestSink(func(r uxrecord.Request) { mu.Lock(); records = append(records, r); mu.Unlock() })

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(`{"model":"omni","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &lastEventRecorder{ResponseRecorder: httptest.NewRecorder(), last: last, got: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Handler()(client, req)
	}()
	select {
	case <-client.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the last event never reached the client")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxied request did not end after the client left")
	}
	_, samples := acct.snapshotAndClearDirty()
	dep := map[string]string{"deployment": itoa(depID)}
	if got, _ := sampleValue(samples, metricPromptTokens, dep); got != 15 {
		t.Errorf("prompt tokens = %v, want the reply's 15", got)
	}
	if got, ok := sampleValue(samples, metricUsageMissing, dep); ok && got != 0 {
		t.Errorf("counted as missing usage: %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 1 || records[0].Outcome != uxrecord.OutcomeOK || records[0].PromptTokens != 15 || records[0].CompletionTokens != 40 {
		t.Fatalf("records = %+v", records)
	}
}

// lastEventRecorder signals once the client has received the event last.
type lastEventRecorder struct {
	*httptest.ResponseRecorder
	last string
	got  chan struct{}
	once sync.Once
}

func (r *lastEventRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	if strings.Contains(r.Body.String(), r.last) {
		r.once.Do(func() { close(r.got) })
	}
	return n, err
}

// firstWriteRecorder signals the first body bytes the client receives.
type firstWriteRecorder struct {
	*httptest.ResponseRecorder
	wrote chan struct{}
	once  sync.Once
}

func (r *firstWriteRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	r.once.Do(func() { close(r.wrote) })
	return n, err
}

// A deployment whose operations report no usage writes no token series.
func TestGatewayWritesNoTokenSeriesWithoutUsage(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3"))
	})
	p, acct, apiKey, depID, closeUpstream := newAccountingProxy(t, upstream)
	defer closeUpstream()
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	p.Handler()(httptest.NewRecorder(), req)

	_, samples := acct.snapshotAndClearDirty()
	for _, metric := range []string{metricPromptTokens, metricUsageMissing, metricTTFTCount} {
		if _, ok := sampleValue(samples, metric, map[string]string{"deployment": itoa(depID)}); ok {
			t.Fatalf("%s written for an audio deployment", metric)
		}
	}
	if got, _ := sampleValue(samples, metricDurationBucket, map[string]string{"deployment": itoa(depID), "le": "1000"}); got != 1 {
		t.Fatalf("its duration is still bucketed: %v", got)
	}
}

func itoa(id uint) string { return strconv.FormatUint(uint64(id), 10) }
