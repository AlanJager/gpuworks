package tunnel_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestLanes_DataLaneDropMidRequestIs503 is the deterministic form of the race
// TestLanes_DataLaneDrop_CtrlSurvives only hits now and then: the request's
// stream is open and the engine is still working on it when the data lane
// drops. The client must get the retryable 503 with Retry-After the dual-lane
// contract promises (ADR 0024), never a 502 that blames an engine which never
// failed.
//
// The request is a POST: net/http does not replay a non-idempotent request on
// its own, and with one instance the retry transport has no other replica to
// switch to, so the status is exactly how the lane loss was classified.
func TestLanes_DataLaneDropMidRequestIs503(t *testing.T) {
	env := testenv.New(t)
	link := env.NewShapedLink(t, testenv.ShapedLinkConfig{
		RateMbps: 50, RTT: 10 * time.Millisecond, SharedRate: true,
	})
	node := env.AddNode("midreq", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithShapedLink(link), testenv.WithServerURL(link.URL()))
	env.WaitNodeOnline(t, node.Name)
	waitDataLane(t, env, node.UUID, "attached")

	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	node.Driver.SetEngineHandler(func(inst testenv.EngineInstance) http.Handler {
		base := bulkEngineHandler(inst)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/chat/completions" {
				base.ServeHTTP(w, r)
				return
			}
			select {
			case arrived <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusOK)
		})
	})
	const served = "midreq-model"
	deployBulkModel(t, env, node, served)
	key := apiKey(t, env)

	type outcome struct {
		status     int
		retryAfter string
		body       string
		err        error
	}
	got := make(chan outcome, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, env.URL()+"/v1/chat/completions", strings.NewReader(`{"model":"`+served+`"}`))
		if err != nil {
			got <- outcome{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			got <- outcome{err: err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		got <- outcome{status: resp.StatusCode, retryAfter: resp.Header.Get("Retry-After"), body: strings.TrimSpace(string(body))}
	}()

	select {
	case <-arrived:
	case <-time.After(15 * time.Second):
		t.Fatal("the request never reached the engine")
	}
	if dropped := link.DropLane("data"); dropped < 1 {
		t.Fatalf("DropLane(data) dropped %d connections", dropped)
	}

	select {
	case o := <-got:
		if o.err != nil {
			t.Fatalf("request transport error: %v", o.err)
		}
		if o.status != http.StatusServiceUnavailable {
			t.Fatalf("status after the data lane dropped mid-request = %d, want 503; body: %s", o.status, o.body)
		}
		if o.retryAfter == "" {
			t.Fatal("the 503 for a lost data lane must carry Retry-After")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the request hung after its data lane dropped")
	}
	if !nodeOnlineInStore(t, env, node.UUID) {
		t.Fatal("node went offline after a data-lane drop")
	}
}
