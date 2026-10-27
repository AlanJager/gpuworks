package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type sseFrame struct {
	at     time.Time
	topic  string
	key    string
	reason string
}

func readSSEFrames(t *testing.T, body *bufio.Reader) <-chan sseFrame {
	t.Helper()
	frames := make(chan sseFrame, 256)
	go func() {
		defer close(frames)
		var topic string
		for {
			line, err := body.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				topic = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var data struct{ Key, Reason string }
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err != nil {
					t.Errorf("SSE data %q: %v", line, err)
					return
				}
				frames <- sseFrame{at: time.Now(), topic: topic, key: data.Key, reason: data.Reason}
			}
		}
	}()
	return frames
}

// collectSSE gathers frames until the stream stays quiet for idle.
func collectSSE(frames <-chan sseFrame, idle time.Duration) []sseFrame {
	var out []sseFrame
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-time.After(idle):
			return out
		}
	}
}

func TestEventsStreamCoalescesRoutineNodeStatus(t *testing.T) {
	old := nodeStatusWindow
	nodeStatusWindow = 400 * time.Millisecond
	defer func() { nodeStatusWindow = old }()

	st, _, router := newSessionRouter(t)
	cookie, _ := sessionLogin(t, router, "password")
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/events", nil)
	req.AddCookie(cookie)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status=%d", response.StatusCode)
	}
	frames := readSSEFrames(t, bufio.NewReader(response.Body))
	if first := <-frames; first.topic != "node" || first.reason != "connected" {
		t.Fatalf("initial frame = %+v", first)
	}

	publish := func(e bus.Event) {
		t.Helper()
		st.Bus.Publish(e)
	}
	start := time.Now()
	for i := 0; i < 30; i++ {
		publish(bus.Event{Topic: "node", Key: fmt.Sprintf("n%02d", i), Reason: store.NodeChangeStatus})
		if i == 10 {
			publish(bus.Event{Topic: "node", Key: "n10", Reason: store.NodeChangeScheduling})
		}
	}
	got := collectSSE(frames, 3*nodeStatusWindow)

	var status []sseFrame
	var scheduling *sseFrame
	for i, f := range got {
		switch {
		case f.topic == "node" && f.reason == store.NodeChangeStatus:
			status = append(status, f)
		case f.topic == "node" && f.reason == store.NodeChangeScheduling:
			scheduling = &got[i]
		default:
			t.Errorf("unexpected frame %+v", f)
		}
	}
	if len(status) != 2 {
		t.Fatalf("status frames = %d, want the first report plus one coalesced flush: %+v", len(status), got)
	}
	if status[0].key != "n00" || status[0].at.Sub(start) > nodeStatusWindow/2 {
		t.Fatalf("first status frame = %+v after %v, want n00 at once", status[0], status[0].at.Sub(start))
	}
	if status[1].key != "" {
		t.Fatalf("coalesced frame names node %q, want none", status[1].key)
	}
	if gap := status[1].at.Sub(status[0].at); gap < nodeStatusWindow*3/4 {
		t.Fatalf("coalesced frame %v after the first, want about one window (%v)", gap, nodeStatusWindow)
	}
	if scheduling == nil || scheduling.key != "n10" || scheduling.at.After(status[1].at) {
		t.Fatalf("scheduling change = %+v, want n10 forwarded before the coalesced flush", scheduling)
	}

	// After a quiet window the next report goes out at once again.
	quietStart := time.Now()
	publish(bus.Event{Topic: "node", Key: "n99", Reason: store.NodeChangeStatus})
	again := collectSSE(frames, 2*nodeStatusWindow)
	if len(again) != 1 || again[0].key != "n99" || again[0].at.Sub(quietStart) > nodeStatusWindow/2 {
		t.Fatalf("report after a quiet window = %+v, want n99 at once", again)
	}
}
