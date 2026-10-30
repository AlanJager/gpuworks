package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// The wait of every held request is counted into the deployment's histogram
// in memory and written with the other gateway series at the flush: exact
// cumulative buckets, a sum and a count, and nothing for a deployment that
// never held a request.
func TestAccountantFlushesHoldWaitsAsAHistogram(t *testing.T) {
	a := newAccountant(nil, 16, nil)
	for _, wait := range []time.Duration{300 * time.Millisecond, 800 * time.Millisecond, 4 * time.Second, 45 * time.Minute} {
		a.RecordHoldWait(7, wait)
	}
	a.Dispatch(8)
	a.Finish(8, 200, time.Second)
	_, samples := a.snapshotAndClearDirty()
	labels := func(le string) map[string]string {
		return map[string]string{"deployment": "7", "node": "server", "le": le}
	}
	for le, want := range map[string]float64{"500": 1, "1000": 2, "2500": 2, "5000": 3, "1800000": 3, "+Inf": 4} {
		if got, ok := sampleValue(samples, metricHoldWaitBucket, labels(le)); !ok || got != want {
			t.Errorf("le=%s = %v (%v), want %v", le, got, ok, want)
		}
	}
	dep := map[string]string{"deployment": "7", "node": "server"}
	if got, _ := sampleValue(samples, metricHoldWaitCount, dep); got != 4 {
		t.Errorf("count = %v, want 4", got)
	}
	if got, _ := sampleValue(samples, metricHoldWaitSum, dep); got != 300+800+4000+45*60*1000 {
		t.Errorf("sum = %v ms", got)
	}
	if _, ok := sampleValue(samples, metricHoldWaitCount, map[string]string{"deployment": "8"}); ok {
		t.Error("a deployment that held nothing has a hold wait series")
	}
	// Cumulative for the life of the process, like every other counter.
	a.RecordHoldWait(7, time.Second)
	_, samples = a.snapshotAndClearDirty()
	if got, _ := sampleValue(samples, metricHoldWaitCount, dep); got != 5 {
		t.Errorf("count after another = %v, want 5", got)
	}
	var nilAccountant *Accountant
	nilAccountant.RecordHoldWait(1, time.Second) // a gateway without accounting counts nothing
}

func holdWaitOf(t *testing.T, a *Accountant, depID uint) (count, sumMS float64) {
	t.Helper()
	_, samples := a.snapshotAndClearDirty()
	dep := map[string]string{"deployment": strconv.Itoa(int(depID)), "node": "server"}
	count, _ = sampleValue(samples, metricHoldWaitCount, dep)
	sumMS, _ = sampleValue(samples, metricHoldWaitSum, dep)
	return count, sumMS
}

// A request held for a wake is counted when it is served, however long it
// waited; one the client abandons is counted too; one refused at once (an
// operator's sleep) was never held and is not.
func TestGatewayCountsTheWaitOfHeldRequests(t *testing.T) {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"ok":true}`) })
	p, acct, key, depID, closeUpstream := newAccountingProxyFor(t, upstream, &store.Deployment{Engine: "vllm", Sleepable: true})
	t.Cleanup(closeUpstream)
	if err := p.store.DB().Model(&store.Instance{}).Where("uuid = ?", "omni-instance").Update("sleep_level", 1).Error; err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p.Handler())
	t.Cleanup(front.Close)

	// Abandoned: the client gives up after 300 ms.
	p.SetWakeDemand(func(uint) {})
	client := &http.Client{Timeout: 300 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	if _, err := client.Do(req); err == nil {
		t.Fatal("answered while asleep")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if n, _ := p.HeldRequests(depID); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned request is still held")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The wait is what was left of the client's 300 ms once the request was
	// held: under load the request may reach the hold late.
	count, abandoned := holdWaitOf(t, acct, depID)
	if count != 1 || abandoned <= 0 || abandoned > 2000 {
		t.Fatalf("abandoned: count = %v sum = %v ms, want one wait of at most about 300 ms", count, abandoned)
	}

	// Served: the wake lands after 200 ms.
	var asked atomic.Int32
	p.SetWakeDemand(func(uint) {
		if asked.Add(1) != 1 {
			return
		}
		go func() {
			time.Sleep(200 * time.Millisecond)
			_ = p.store.DB().Model(&store.Instance{}).Where("uuid = ?", "omni-instance").Update("sleep_level", 0).Error
		}()
	})
	req, _ = http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"omni"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if count, sum := holdWaitOf(t, acct, depID); count != 2 || sum-abandoned < 200 {
		t.Fatalf("served: count = %v sum = %v ms, want a second wait of at least 200 ms", count, sum)
	}

	// Refused at once: an operator's sleep is not held.
	if err := p.store.DB().Model(&store.Instance{}).Where("uuid = ?", "omni-instance").Update("sleep_level", 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := p.store.DB().Model(&store.Deployment{}).Where("id = ?", depID).Update("sleep_held", true).Error; err != nil {
		t.Fatal(err)
	}
	if resp := postChat(t, front, key); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("held sleep status = %d", resp.StatusCode)
	}
	if count, _ := holdWaitOf(t, acct, depID); count != 2 {
		t.Fatalf("a refused request was counted as held: %v", count)
	}
}
