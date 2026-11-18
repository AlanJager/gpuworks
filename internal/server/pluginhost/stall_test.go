package pluginhost

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// fakeClock is a settable clock for the meter.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// A connection whose reader stops: the write blocks, and once it has been
// blocked longer than the limit the meter says so; when the reader
// resumes the stall is gone.
func TestAPipeReaderThatStopsIsDetected(t *testing.T) {
	clock := newFakeClock()
	m := newStallMeter(clock.now)
	client, plugin := net.Pipe()
	defer client.Close()
	defer plugin.Close()
	conn := &meteredConn{Conn: client, m: m, scan: newFrameScan()}

	wrote := make(chan error, 1)
	go func() {
		_, err := conn.Write(make([]byte, 4096))
		wrote <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for m.writing.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the write never started")
		}
		time.Sleep(time.Millisecond)
	}
	if d, stalled := m.stalled(time.Second); stalled || d != 0 {
		t.Fatalf("a write that just started = %v, %v", d, stalled)
	}
	clock.advance(1500 * time.Millisecond)
	if d, stalled := m.stalled(time.Second); !stalled || d != 1500*time.Millisecond {
		t.Fatalf("a write blocked 1.5 s = %v, %v", d, stalled)
	}
	if _, stalled := m.stalled(2 * time.Second); stalled {
		t.Fatal("a write blocked 1.5 s stalls a 2 s limit")
	}

	go func() { _, _ = io.Copy(io.Discard, plugin) }()
	if err := <-wrote; err != nil {
		t.Fatal(err)
	}
	if d, stalled := m.stalled(time.Second); stalled || d != 0 || m.writing.Load() != 0 {
		t.Fatalf("after the reader resumed = %v, %v (writing %d)", d, stalled, m.writing.Load())
	}
}

// Only DATA payload is body progress: the preface, settings, headers and
// pings are not, wherever the writes split the frames.
func TestFrameScanCountsOnlyDataPayload(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString(http2.ClientPreface)
	fr := http2.NewFramer(&buf, nil)
	_ = fr.WriteSettings()
	_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: 1, BlockFragment: []byte("headers"), EndHeaders: true})
	_ = fr.WriteData(1, false, bytes.Repeat([]byte("a"), 100))
	_ = fr.WritePing(false, [8]byte{1, 2, 3, 4, 5, 6, 7, 8})
	_ = fr.WriteData(1, true, bytes.Repeat([]byte("b"), 50))
	_ = fr.WriteWindowUpdate(0, 1000)
	stream := buf.Bytes()

	for _, split := range []int{1, 2, 7, 9, 10, 64, len(stream)} {
		s := newFrameScan()
		got := 0
		for p := stream; len(p) > 0; {
			k := min(split, len(p))
			got += s.data(p[:k])
			p = p[k:]
		}
		if got != 150 {
			t.Fatalf("split %d: %d DATA payload bytes, want 150", split, got)
		}
	}
	s := newFrameScan()
	if got := s.data(stream[:len(http2.ClientPreface)+9+6]); got != 0 {
		t.Fatalf("preface and settings = %d DATA bytes", got)
	}
}

// Body bytes the client read and cannot send stall the connection once
// nothing is written for longer than the limit; a DATA frame written or
// the next read is progress, and a finished body holds nothing.
func TestPendingBodyBytesWithoutProgressStall(t *testing.T) {
	clock := newFakeClock()
	m := newStallMeter(clock.now)
	conn := &meteredConn{Conn: discardConn{}, m: m, scan: newFrameScan()}
	_, _ = conn.Write([]byte(http2.ClientPreface))
	b := newMeteredBody(io.NopCloser(bytes.NewReader(make([]byte, 64))), -1, m)

	p := make([]byte, 16)
	if n, _ := b.Read(p); n != 16 || m.unsent.Load() != 16 {
		t.Fatalf("read %d, unsent %d", n, m.unsent.Load())
	}
	clock.advance(3 * time.Second)
	if d, stalled := m.stalled(2 * time.Second); !stalled || d != 3*time.Second {
		t.Fatalf("16 bytes held 3 s = %v, %v", d, stalled)
	}

	// A DATA frame reaches the connection: progress.
	var frame bytes.Buffer
	_ = http2.NewFramer(&frame, nil).WriteData(1, false, p[:8])
	_, _ = conn.Write(frame.Bytes())
	if _, stalled := m.stalled(2 * time.Second); stalled {
		t.Fatal("still stalled after a DATA frame was written")
	}
	clock.advance(3 * time.Second)
	if _, stalled := m.stalled(2 * time.Second); !stalled {
		t.Fatal("not stalled 3 s after the last DATA frame")
	}

	// The next read means the held chunk was written.
	if n, _ := b.Read(p); n != 16 || m.unsent.Load() != 16 {
		t.Fatalf("second read %d, unsent %d", n, m.unsent.Load())
	}
	if _, stalled := m.stalled(2 * time.Second); stalled {
		t.Fatal("stalled right after a chunk was sent")
	}

	// Written out: nothing pending, however long nothing is written.
	b.finish(true)
	clock.advance(time.Hour)
	if d, stalled := m.stalled(2 * time.Second); stalled || m.unsent.Load() != 0 || d != 0 {
		t.Fatalf("after finish = %v, %v, unsent %d", d, stalled, m.unsent.Load())
	}
	if n, _ := b.Read(p); n != 16 || m.unsent.Load() != 0 {
		t.Fatalf("a read after finish holds %d", m.unsent.Load())
	}

	// A body abandoned by Close gives back what it held.
	other := newMeteredBody(io.NopCloser(bytes.NewReader(make([]byte, 64))), -1, m)
	_, _ = other.Read(p)
	if err := other.Close(); err != nil || m.unsent.Load() != 0 {
		t.Fatalf("close = %v, unsent %d", err, m.unsent.Load())
	}
}

// Bytes that become pending after none were start their wait at a time
// read before they are counted, so a check running alongside never pairs
// them with an older progress time, however old (or unset) it is.
func TestHeldBytesNeverPairWithAnOldProgressTime(t *testing.T) {
	for _, old := range []int64{0, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC).UnixNano()} {
		clock := newFakeClock()
		var m *stallMeter
		var unsentAtClock []int64
		m = newStallMeter(func() time.Time {
			unsentAtClock = append(unsentAtClock, m.unsent.Load())
			return clock.now()
		})
		m.progress.Store(old)
		m.hold(16)
		if len(unsentAtClock) != 1 || unsentAtClock[0] != 0 {
			t.Fatalf("progress %d: the wait's start was read with %v bytes already counted", old, unsentAtClock)
		}
		if d, stalled := m.stalled(time.Second); stalled || d != 0 {
			t.Fatalf("progress %d: fresh held bytes = %v, %v", old, d, stalled)
		}
	}
}

// The per-write and per-read counters allocate nothing.
func TestStallCountersDoNotAllocate(t *testing.T) {
	m := newStallMeter(nil)
	conn := &meteredConn{Conn: discardConn{}, m: m, scan: newFrameScan()}
	var frame bytes.Buffer
	_ = http2.NewFramer(&frame, nil).WriteData(1, false, make([]byte, 512))
	data := frame.Bytes()
	_, _ = conn.Write([]byte(http2.ClientPreface))
	src := &endless{}
	b := newMeteredBody(src, -1, m)
	p := make([]byte, 256)
	allocs := testing.AllocsPerRun(1000, func() {
		_, _ = conn.Write(data)
		_, _ = b.Read(p)
		_, _ = m.stalled(time.Second)
	})
	if allocs != 0 {
		t.Fatalf("%v allocations per write, read and check", allocs)
	}
}

type discardConn struct{ net.Conn }

func (discardConn) Write(p []byte) (int, error) { return len(p), nil }

type endless struct{}

func (*endless) Read(p []byte) (int, error) { return len(p), nil }
func (*endless) Close() error               { return nil }

func stallTiming() Timing {
	t := fastTiming()
	t.StallAfter = 300 * time.Millisecond
	return t
}

func postBody(ctx context.Context, h *Host, id string, size int) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, "/upload", bytes.NewReader(make([]byte, size)))
	return h.Do(ctx, id, req, Class{})
}

// A plugin that never reads a request body larger than the HTTP/2 windows
// is restarted with PLUGIN_STALLED while its health probes still pass
// (they would otherwise have restarted it with PLUGIN_UNHEALTHY first);
// the call it stalled fails, and its next process serves.
func TestAPluginWithUnreadBodiesIsRestartedWhileItsHealthPasses(t *testing.T) {
	// Probes the stalled process answered while it held the body.
	var stuck atomic.Bool
	var probesWhileStuck atomic.Int32
	sp := &fakeSpawner{config: func(n int) pluginsdk.Config {
		c := sdkConfig("acme_upload")
		if n == 1 {
			c.Health = func(context.Context) extapi.Health {
				if stuck.Load() {
					probesWhileStuck.Add(1)
				}
				return extapi.Health{Status: extapi.HealthOK}
			}
		}
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				stuck.Store(true)
				<-r.Context().Done() // never reads the body
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, "ok")
		})
		return c
	}}
	h, log := newTestHost(t, sp, nil, stallTiming())
	if err := h.Start(testSpec(t, "acme_upload")); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, h, "acme_upload", inState(StateRunning))
	started := time.Now()
	if _, err := postBody(context.Background(), h, "acme_upload", 8<<20); err == nil {
		t.Fatal("the stalled call succeeded")
	} else if code, _ := productParams(t, err); code != messages.ErrorPluginRouteFailed {
		t.Fatalf("the stalled call = %v", err)
	}
	if took := time.Since(started); took < 300*time.Millisecond || took > 4*time.Second {
		t.Fatalf("the stall was noticed after %v", took)
	}
	waitStatus(t, h, "acme_upload", func(s Status) bool { return s.State == StateRunning && s.Starts == 2 })
	if sent := sp.proc(1).sent(); !slices.Contains(sent, os.Signal(syscall.SIGKILL)) {
		t.Fatalf("the stalled plugin got %v", sent)
	}
	var stalled *Status
	log.mu.Lock()
	for _, s := range log.all {
		if s.Code == messages.ErrorPluginUnhealthy {
			t.Errorf("the stalled plugin failed its health probes: %+v", s)
		}
		if s.State == StateBackoff && s.Code == messages.ErrorPluginStalled {
			stalled = &s
		}
	}
	log.mu.Unlock()
	if stalled == nil || stalled.Params["id"] != "acme_upload" || stalled.Params["stalled_s"] != 1 {
		t.Fatalf("no backoff named PLUGIN_STALLED{id, stalled_s: 1}: %+v", stalled)
	}
	if n := probesWhileStuck.Load(); n < 2 {
		t.Fatalf("the stalled plugin answered %d health probes while it held the body", n)
	}

	resp, err := postBody(context.Background(), h, "acme_upload", 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("the restarted plugin answered %q", body)
	}
}

// Small bodies the plugin never reads fill the connection's window too:
// each fits in one chunk the client reads whole (it checks for the end
// with one more read before it waits for flow control), so the last
// chunk read stays pending until it is written, and the next call's body
// stalls the connection.
func TestSmallUnreadBodiesThatFillTheWindowStall(t *testing.T) {
	sp := &fakeSpawner{config: func(n int) pluginsdk.Config {
		c := sdkConfig("acme_upload")
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if n == 1 {
				<-r.Context().Done() // never reads the body
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = io.WriteString(w, "ok")
		})
		return c
	}}
	h, log := newTestHost(t, sp, nil, stallTiming())
	spec := testSpec(t, "acme_upload")
	spec.MaxConcurrency = 64
	if err := h.Start(spec); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, h, "acme_upload", inState(StateRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 21 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := postBody(ctx, h, "acme_upload", 100<<10); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	waitStatus(t, h, "acme_upload", func(s Status) bool { return s.Starts == 2 })
	cancel()
	wg.Wait()
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, s := range log.all {
		if s.State == StateBackoff && s.Code == messages.ErrorPluginStalled {
			return
		}
	}
	t.Fatalf("no backoff named PLUGIN_STALLED: %+v", log.all)
}

// A plugin that reads a large body slowly, but keeps reading, is never
// restarted, though the body waits on flow control far longer in total
// than the stall limit.
func TestASlowPluginThatKeepsReadingIsNotRestarted(t *testing.T) {
	const size = 3 << 20
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_upload")
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf := make([]byte, 32<<10)
			total := 0
			for {
				n, err := io.ReadFull(r.Body, buf)
				total += n
				if err != nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if total != size {
				http.Error(w, "short body", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(w, "ok")
		})
		return c
	}}
	h, _ := newTestHost(t, sp, nil, stallTiming())
	if err := h.Start(testSpec(t, "acme_upload")); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, h, "acme_upload", inState(StateRunning))

	// Watch the meter so the test proves bodies did wait on flow control.
	var maxUnsent atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if r := h.get("acme_upload").currentRun(); r != nil {
				if u := r.conn.meter.unsent.Load(); u > maxUnsent.Load() {
					maxUnsent.Store(u)
				}
			}
		}
	}()
	started := time.Now()
	resp, err := postBody(context.Background(), h, "acme_upload", size)
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("the slow plugin answered %d %q", resp.StatusCode, body)
	}
	if took := time.Since(started); took < 2*stallTiming().StallAfter {
		t.Fatalf("the body took %v, too fast to prove anything", took)
	}
	if maxUnsent.Load() == 0 {
		t.Fatal("no body bytes ever waited on flow control")
	}
	if st, _ := h.Status("acme_upload"); st.Starts != 1 || st.State != StateRunning {
		t.Fatalf("the slow plugin was restarted: %+v", st)
	}
}
