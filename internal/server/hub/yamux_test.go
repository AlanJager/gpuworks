package hub

import (
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestLaneYamuxConfig_MatchesPlan is a negative-control regression guard: it
// pins the yamux tuning to the values the adaptive-bandwidth tunnel plan
// specifies (§3.1). A silent revert of the window ceiling would drop
// single-stream throughput back to ~87 Mbps at 20 ms RTT, which the throughput
// acceptance test would then fail — this catches the same regression cheaply.
func TestLaneYamuxConfig_MatchesPlan(t *testing.T) {
	data := laneYamuxConfig(proto.LaneData, io.Discard)
	if data.InitialStreamWindowSize != 256*1024 {
		t.Errorf("data initial window = %d, want 256 KiB", data.InitialStreamWindowSize)
	}
	if data.MaxStreamWindowSize != 16*1024*1024 {
		t.Errorf("data max window = %d, want 16 MiB", data.MaxStreamWindowSize)
	}
	if data.MaxIncomingStreams != 4096 {
		t.Errorf("data max incoming streams = %d, want 4096", data.MaxIncomingStreams)
	}
	if !data.EnableKeepAlive {
		t.Error("keepalive disabled on data lane")
	}
	if data.KeepAliveInterval != 10*time.Second {
		t.Errorf("data keepalive interval = %s, want 10s", data.KeepAliveInterval)
	}
	if data.ConnectionWriteTimeout != 30*time.Second {
		t.Errorf("data connection write timeout = %s, want 30s", data.ConnectionWriteTimeout)
	}

	ctrl := laneYamuxConfig(proto.LaneCtrl, io.Discard)
	if ctrl.InitialStreamWindowSize != 256*1024 || ctrl.MaxStreamWindowSize != 256*1024 {
		t.Errorf("ctrl window = %d/%d, want fixed 256 KiB/256 KiB",
			ctrl.InitialStreamWindowSize, ctrl.MaxStreamWindowSize)
	}
	if ctrl.MaxIncomingStreams != 256 {
		t.Errorf("ctrl max incoming streams = %d, want 256", ctrl.MaxIncomingStreams)
	}
	if ctrl.KeepAliveInterval != 10*time.Second || ctrl.ConnectionWriteTimeout != 30*time.Second {
		t.Errorf("ctrl keepalive/write timeout = %s/%s, want 10s/30s",
			ctrl.KeepAliveInterval, ctrl.ConnectionWriteTimeout)
	}
	for name, cfg := range map[string]*yamux.Config{"data": data, "ctrl": ctrl} {
		assertKeepAliveClearOfWriteDeadline(t, name, cfg)
	}

	// The config must satisfy go-yamux's own validation (min window sizes etc.),
	// which Server/Client enforce before use.
	if err := yamux.VerifyConfig(data); err != nil {
		t.Fatalf("data lane config rejected by go-yamux: %v", err)
	}
	if err := yamux.VerifyConfig(ctrl); err != nil {
		t.Fatalf("ctrl lane config rejected by go-yamux: %v", err)
	}
}

// keepAliveDeadlineMargin is how far short of half the connection write timeout
// the keepalive interval must stay. go-yamux re-arms the write deadline only
// once half the timeout has elapsed since the last re-arm, so on an idle lane
// the keepalive ping is the write that re-arms it; with the interval equal to
// half the timeout that ping lands within a millisecond of the deadline every
// cycle and races the deadline callback in coder/websocket's net.Conn, which
// closes the connection when it catches a write in flight. Five seconds is far
// beyond any scheduling jitter.
const keepAliveDeadlineMargin = 5 * time.Second

func assertKeepAliveClearOfWriteDeadline(t *testing.T, lane string, cfg *yamux.Config) {
	t.Helper()
	if !cfg.EnableKeepAlive {
		t.Errorf("%s lane: keepalive disabled", lane)
		return
	}
	if cfg.KeepAliveInterval+keepAliveDeadlineMargin > cfg.ConnectionWriteTimeout/2 {
		t.Errorf("%s lane: keepalive interval %s is within %s of half the write timeout %s; the keepalive write would race the write deadline",
			lane, cfg.KeepAliveInterval, keepAliveDeadlineMargin, cfg.ConnectionWriteTimeout)
	}
}
