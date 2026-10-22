package conn

import (
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestLaneYamuxConfig_KeepsKeepAliveClearOfTheWriteDeadline pins the agent
// side of the lane tuning to the server's (internal/server/hub/yamux.go) and
// guards the one relation that matters for session survival: the keepalive
// interval must stay well short of half the connection write timeout.
//
// go-yamux re-arms the write deadline only once half the timeout has elapsed
// since the last re-arm, so on an idle lane the keepalive ping is the write
// that re-arms it. With the two equal, that ping lands within a millisecond of
// the deadline every cycle and races the deadline callback in coder/websocket's
// net.Conn, which cancels the write context for good and closes the connection
// when it catches a write in flight. That dropped every lane about four times
// an hour before the interval was moved to 10 s.
func TestLaneYamuxConfig_KeepsKeepAliveClearOfTheWriteDeadline(t *testing.T) {
	const margin = 5 * time.Second
	for _, lane := range []string{proto.LaneCtrl, proto.LaneData} {
		cfg := laneYamuxConfig(lane, io.Discard)
		if !cfg.EnableKeepAlive {
			t.Errorf("%s lane: keepalive disabled", lane)
		}
		if cfg.KeepAliveInterval != 10*time.Second || cfg.ConnectionWriteTimeout != 30*time.Second {
			t.Errorf("%s lane keepalive/write timeout = %s/%s, want 10s/30s (must match the server)",
				lane, cfg.KeepAliveInterval, cfg.ConnectionWriteTimeout)
		}
		if cfg.KeepAliveInterval+margin > cfg.ConnectionWriteTimeout/2 {
			t.Errorf("%s lane: keepalive interval %s is within %s of half the write timeout %s; the keepalive write would race the write deadline",
				lane, cfg.KeepAliveInterval, margin, cfg.ConnectionWriteTimeout)
		}
		if err := yamux.VerifyConfig(cfg); err != nil {
			t.Fatalf("%s lane config rejected by go-yamux: %v", lane, err)
		}
	}

	ctrl := laneYamuxConfig(proto.LaneCtrl, io.Discard)
	if ctrl.InitialStreamWindowSize != ctrlLaneWindow || ctrl.MaxStreamWindowSize != ctrlLaneWindow {
		t.Errorf("ctrl window = %d/%d, want fixed %d", ctrl.InitialStreamWindowSize, ctrl.MaxStreamWindowSize, ctrlLaneWindow)
	}
	data := laneYamuxConfig(proto.LaneData, io.Discard)
	if data.InitialStreamWindowSize != dataLaneInitialWindow || data.MaxStreamWindowSize != dataLaneMaxWindow {
		t.Errorf("data window = %d/%d, want %d/%d", data.InitialStreamWindowSize, data.MaxStreamWindowSize, dataLaneInitialWindow, dataLaneMaxWindow)
	}
}
