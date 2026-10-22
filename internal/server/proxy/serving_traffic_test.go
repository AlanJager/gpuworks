package proxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

func roleRates(batch []tsdb.Sample) map[string]float64 {
	out := map[string]float64{}
	for _, s := range batch {
		if s.Metric != metricRoleRxBytes && s.Metric != metricRoleTxBytes {
			continue
		}
		if s.Labels["role"] != roleServing {
			panic("role label = " + s.Labels["role"])
		}
		out[s.Labels["node"]+" "+s.Metric] = s.Value
	}
	return out
}

func TestServingTrafficIsReportedPerNodeFromTheNodesSide(t *testing.T) {
	a := newAccountant(nil, 4, nil)
	clock := time.Unix(10_000, 0)
	a.now = func() time.Time { return clock }

	gateway, node := net.Pipe()
	defer node.Close()
	conn := a.countServing(gateway, "node-a")
	go func() {
		buf := make([]byte, 300)
		_, _ = io.ReadFull(node, buf)
		_, _ = node.Write(make([]byte, 5_000))
	}()
	// The baseline flush reports nothing yet.
	a.flush()
	select {
	case batch := <-a.queue:
		if got := roleRates(batch); len(got) != 0 {
			t.Fatalf("baseline flush = %v", got)
		}
	default:
	}

	if _, err := conn.Write(make([]byte, 300)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, make([]byte, 5_000)); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(10 * time.Second)
	a.flush()
	got := roleRates(<-a.queue)
	want := map[string]float64{"node-a " + metricRoleRxBytes: 30, "node-a " + metricRoleTxBytes: 500}
	if len(got) != 2 || got["node-a "+metricRoleRxBytes] != 30 || got["node-a "+metricRoleTxBytes] != 500 {
		t.Fatalf("rates = %v, want %v", got, want)
	}

	// An idle node keeps reporting zero instead of vanishing from the chart.
	clock = clock.Add(10 * time.Second)
	a.flush()
	got = roleRates(<-a.queue)
	if len(got) != 2 || got["node-a "+metricRoleRxBytes] != 0 || got["node-a "+metricRoleTxBytes] != 0 {
		t.Fatalf("idle rates = %v", got)
	}
}

func TestCountServingWithoutAnAccountantPassesTheConnThrough(t *testing.T) {
	gateway, node := net.Pipe()
	defer gateway.Close()
	defer node.Close()
	var a *Accountant
	if a.countServing(gateway, "node-a") != gateway {
		t.Fatal("a nil accountant must not wrap the connection")
	}
	b := newAccountant(nil, 4, nil)
	if b.countServing(gateway, "") != gateway || b.countServing(nil, "node-a") != nil {
		t.Fatal("no node or no connection means nothing to count")
	}
}

func TestProxiedRequestsCountAsTheNodesServingTraffic(t *testing.T) {
	body := strings.Repeat("x", 4_000)
	p, acct, apiKey, _, closeUpstream := newAccountingProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, body)
	}))
	defer closeUpstream()
	clock := time.Unix(20_000, 0)
	acct.now = func() time.Time { return clock }
	acct.flush() // baseline
	for len(acct.queue) > 0 {
		<-acct.queue
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"omni","prompt":"`+strings.Repeat("p", 1_000)+`"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	p.Handler()(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != len(body) {
		t.Fatalf("proxied response = %d, %d bytes", rec.Code, rec.Body.Len())
	}

	// The transport's write goroutine may count the request body just after
	// the response is back; those bytes land in the next interval, so add up
	// intervals until both directions are in.
	var rx, tx float64
	for i := 0; i < 50 && (rx < 1_000 || tx < 4_000); i++ {
		clock = clock.Add(time.Second)
		acct.flush()
		got := roleRates(<-acct.queue)
		rx += got["node-omni "+metricRoleRxBytes]
		tx += got["node-omni "+metricRoleTxBytes]
		time.Sleep(10 * time.Millisecond)
	}
	// The request (with headers) went to the node, the response came back.
	if rx < 1_000 || rx > 2_000 {
		t.Fatalf("node rx = %v bytes, want the ~1 KB request", rx)
	}
	if tx < 4_000 || tx > 5_000 {
		t.Fatalf("node tx = %v bytes, want the ~4 KB response", tx)
	}
}
