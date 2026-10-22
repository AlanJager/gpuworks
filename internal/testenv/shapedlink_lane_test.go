package testenv

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// streamServer accepts connections and writes payload bytes to each, then
// half-closes so the reader sees EOF.
func startStreamServer(t *testing.T, payload int) (string, func()) {
	t.Helper()
	data := make([]byte, payload)
	return startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		_, _ = c.Write(data)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		_, _ = io.Copy(io.Discard, c)
	})
}

// downloadMbps dials the link, reads payload bytes, and returns the measured
// receive rate in Mbps.
func downloadMbps(t *testing.T, addr string, payload int) float64 {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	n, err := io.Copy(io.Discard, conn)
	if err != nil || n != int64(payload) {
		t.Fatalf("download n=%d err=%v want %d", n, err, payload)
	}
	return float64(payload*8) / 1e6 / time.Since(start).Seconds()
}

// TestShapedLink_SharedRateSplitsBandwidth proves the SharedRate toggle: two
// concurrent downloads through a 20 Mbps shared link share one 20 Mbps pipe
// (aggregate ~20 Mbps, and the flow that finishes first was held to about half),
// while with SharedRate off each connection has its own private 20 Mbps pipe
// (aggregate ~40 Mbps). The scheduler is work-conserving, so the flow that
// outlives the other is allowed to speed up once it is alone; per-flow lifetime
// rates are therefore not asserted individually. The shared mode is what the
// dual-lane tests need: otherwise the ctrl lane would get a free private pipe
// and the latency assertions would be trivially satisfied.
func TestShapedLink_SharedRateSplitsBandwidth(t *testing.T) {
	const payload = 1 << 20 // 1 MiB per flow: ~0.8 s total at 20 Mbps shared
	target, stop := startStreamServer(t, payload)
	defer stop()
	for _, tc := range []struct {
		name          string
		shared        bool
		aggLow, aggHi float64 // aggregate Mbps band
		minFlowHi     float64 // the slower flow must be at or below this
	}{
		{"shared", true, 14, 27, 14},
		{"per_connection", false, 28, 54, 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, err := NewShapedLink(target, ShapedLinkConfig{RateMbps: 20, SharedRate: tc.shared})
			if err != nil {
				t.Fatal(err)
			}
			defer link.Close()
			var wg sync.WaitGroup
			rates := make([]float64, 2)
			start := time.Now()
			for i := range rates {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					rates[i] = downloadMbps(t, link.Addr(), payload)
				}(i)
			}
			wg.Wait()
			agg := float64(2*payload*8) / 1e6 / time.Since(start).Seconds()
			minRate := rates[0]
			if rates[1] < minRate {
				minRate = rates[1]
			}
			t.Logf("aggregate %.1f Mbps, per-flow %v", agg, rates)
			if agg < tc.aggLow || agg > tc.aggHi {
				t.Fatalf("aggregate %.1f Mbps, want within [%.0f, %.0f] (flows=%v)", agg, tc.aggLow, tc.aggHi, rates)
			}
			if minRate > tc.minFlowHi {
				t.Fatalf("slower flow %.1f Mbps exceeds %.0f: flows were not sharing the link (flows=%v)", minRate, tc.minFlowHi, rates)
			}
		})
	}
}

// TestShapedLink_SharedRateIdleFlowNotStarved proves the fair-share clock serves
// a connection with no backlog immediately: while one connection floods a
// 10 Mbps shared link in both directions, a small echo on a second connection
// completes well under the time the flood's queued bytes would take to drain.
// This is the mechanism that lets the ctrl lane stay responsive under a
// saturated data lane.
func TestShapedLink_SharedRateIdleFlowNotStarved(t *testing.T) {
	target, stop := startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		_, _ = io.Copy(c, c) // echo
	})
	defer stop()
	link, err := NewShapedLink(target, ShapedLinkConfig{RateMbps: 10, SharedRate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	// Flood: 4 MiB written at once (~3.4 s to serialize at 10 Mbps).
	flood, err := net.Dial("tcp", link.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer flood.Close()
	go func() { _, _ = flood.Write(make([]byte, 4<<20)) }()
	go func() { _, _ = io.Copy(io.Discard, flood) }()
	time.Sleep(300 * time.Millisecond) // let the flood's clock run ahead

	small, err := net.Dial("tcp", link.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	start := time.Now()
	if _, err := small.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = small.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(small, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo err=%v got=%q", err, buf)
	}
	if rtt := time.Since(start); rtt > 250*time.Millisecond {
		t.Fatalf("idle flow echo took %v behind a flood; fair share should serve it promptly", rtt)
	}
}

// TestShapedLink_DropLaneSeversOnlyTaggedLane proves lane tagging and selective
// drop: connections are tagged from the ?lane= query in their first request
// line, DropLane("data") closes only the data-tagged connection, and the
// ctrl-tagged connection keeps working.
func TestShapedLink_DropLaneSeversOnlyTaggedLane(t *testing.T) {
	target, stop := startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		_, _ = io.Copy(c, c) // echo everything, including the request line
	})
	defer stop()
	link, err := NewShapedLink(target, ShapedLinkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	open := func(lane string) net.Conn {
		c, err := net.Dial("tcp", link.Addr())
		if err != nil {
			t.Fatalf("dial %s: %v", lane, err)
		}
		req := fmt.Sprintf("GET /api/v1/agent/connect?lane=%s HTTP/1.1\r\nHost: x\r\n\r\n", lane)
		if _, err := c.Write([]byte(req)); err != nil {
			t.Fatalf("write %s: %v", lane, err)
		}
		// Drain the echoed request so later reads see only new data.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(c, make([]byte, len(req))); err != nil {
			t.Fatalf("echo %s: %v", lane, err)
		}
		_ = c.SetReadDeadline(time.Time{})
		return c
	}
	ctrlConn := open("ctrl")
	defer ctrlConn.Close()
	dataConn := open("data")
	defer dataConn.Close()
	if got := link.countLane("ctrl") + link.countLane("data"); got != 2 {
		t.Fatalf("tagged conns = %d (ctrl=%d data=%d untagged=%d)", got, link.countLane("ctrl"), link.countLane("data"), link.countLane(""))
	}

	if dropped := link.DropLane("data"); dropped != 1 {
		t.Fatalf("DropLane(data) closed %d conns, want 1", dropped)
	}
	_ = dataConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := dataConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("data connection still readable after DropLane(data)")
	}
	// The ctrl connection is untouched and still echoes.
	if _, err := ctrlConn.Write([]byte("still-here")); err != nil {
		t.Fatalf("ctrl write after data drop: %v", err)
	}
	_ = ctrlConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len("still-here"))
	if _, err := io.ReadFull(ctrlConn, buf); err != nil || string(buf) != "still-here" {
		t.Fatalf("ctrl echo after data drop err=%v got=%q", err, buf)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && link.countLane("data") != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := link.countLane("data"); got != 0 {
		t.Fatalf("data lane still tracked after drop: %d", got)
	}
	if got := link.countLane("ctrl"); got != 1 {
		t.Fatalf("ctrl lane disturbed by DropLane(data): %d", got)
	}
}
