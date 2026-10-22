package conn

import (
	"io"
	"time"

	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Lane yamux tuning, mirroring the server side (internal/server/hub/yamux.go).
// Both ends must agree on keepalive and window behaviour; the values come from
// the adaptive-bandwidth tunnel plan (§3.1).
//
// The data lane lets go-yamux auto-tune the receive window from 256 KiB up to
// 16 MiB so a single stream can fill a high bandwidth-delay-product link. The
// ctrl lane pins the window at 256 KiB for small, latency-sensitive traffic.
//
// The keepalive interval must stay well short of half the connection write
// timeout; the server-side file explains the race with coder/websocket's write
// deadline that an equal ratio provokes. The margin on this side is guarded by
// TestLaneYamuxConfig_KeepsKeepAliveClearOfTheWriteDeadline.
const (
	ctrlLaneWindow        = 256 * 1024
	dataLaneInitialWindow = 256 * 1024
	dataLaneMaxWindow     = 16 * 1024 * 1024
	laneKeepAliveInterval = 10 * time.Second
	laneConnWriteTimeout  = 30 * time.Second
	ctrlLaneMaxStreams    = 256
	dataLaneMaxStreams    = 4096
)

// laneYamuxConfig builds the go-yamux configuration for a lane. A single-lane
// agent dials one session with the data-lane profile; a dual-lane agent also
// calls this with proto.LaneCtrl for the dedicated control connection.
func laneYamuxConfig(lane string, logOut io.Writer) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = laneKeepAliveInterval
	cfg.ConnectionWriteTimeout = laneConnWriteTimeout
	if logOut != nil {
		cfg.LogOutput = logOut
	}
	switch lane {
	case proto.LaneCtrl:
		cfg.InitialStreamWindowSize = ctrlLaneWindow
		cfg.MaxStreamWindowSize = ctrlLaneWindow
		cfg.MaxIncomingStreams = ctrlLaneMaxStreams
	default: // proto.LaneData
		cfg.InitialStreamWindowSize = dataLaneInitialWindow
		cfg.MaxStreamWindowSize = dataLaneMaxWindow
		cfg.MaxIncomingStreams = dataLaneMaxStreams
	}
	return cfg
}
