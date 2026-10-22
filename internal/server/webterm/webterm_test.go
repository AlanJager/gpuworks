package webterm

import (
	"errors"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeSessions records audit calls without a database.
type fakeSessions struct {
	mu        sync.Mutex
	cfg       store.TerminalSettings
	nextID    uint
	created   []store.TerminalSession
	closed    []closedRecord
	createErr error
}

type closedRecord struct {
	ID       uint
	BytesIn  int64
	BytesOut int64
	Reason   string
}

func newFake(cfg store.TerminalSettings) *fakeSessions {
	return &fakeSessions{cfg: cfg}
}

func (f *fakeSessions) TerminalSettings() store.TerminalSettings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakeSessions) setSettings(cfg store.TerminalSettings) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cfg = cfg
}

func (f *fakeSessions) CreateTerminalSession(s *store.TerminalSession) (*store.TerminalSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.nextID++
	s.ID = f.nextID
	f.created = append(f.created, *s)
	return s, nil
}

func (f *fakeSessions) CloseTerminalSession(id uint, in, out int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, closedRecord{ID: id, BytesIn: in, BytesOut: out, Reason: reason})
	return nil
}

func (f *fakeSessions) closedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.closed)
}

func req(node string) Request {
	return Request{UserID: 1, Username: "admin", NodeUUID: node, Target: "host", ClientIP: "10.0.0.1"}
}

// The kill switch has to be absolute: this feature exposes a root shell on every
// GPU node through a browser.
func TestBeginRejectsWhenDisabled(t *testing.T) {
	cfg := store.DefaultTerminalSettings()
	cfg.Enabled = false
	f := newFake(cfg)
	m := New(f)

	if _, err := m.Begin(req("node-a")); !errors.Is(err, ErrDisabled) {
		t.Fatalf("err = %v, want ErrDisabled", err)
	}
	if len(f.created) != 0 {
		t.Errorf("created %d audit records for a rejected session, want 0", len(f.created))
	}
}

func TestBeginEnforcesPerNodeLimit(t *testing.T) {
	cfg := store.DefaultTerminalSettings()
	cfg.MaxPerNode = 2
	cfg.MaxGlobal = 10
	m := New(newFake(cfg))

	var open []*Session
	for i := 0; i < cfg.MaxPerNode; i++ {
		s, err := m.Begin(req("node-a"))
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		open = append(open, s)
	}
	if _, err := m.Begin(req("node-a")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("err = %v, want ErrNodeBusy", err)
	}

	// A different node has its own budget.
	other, err := m.Begin(req("node-b"))
	if err != nil {
		t.Fatalf("other node: %v", err)
	}

	// Closing frees the slot.
	if err := open[0].Close(ReasonClientClosed); err != nil {
		t.Fatalf("close: %v", err)
	}
	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	_ = s.Close(ReasonClientClosed)
	_ = other.Close(ReasonClientClosed)
	for _, o := range open[1:] {
		_ = o.Close(ReasonClientClosed)
	}
}

func TestBeginEnforcesGlobalLimit(t *testing.T) {
	cfg := store.DefaultTerminalSettings()
	cfg.MaxPerNode = 4
	cfg.MaxGlobal = 3
	m := New(newFake(cfg))

	// Spread across nodes so the per-node limit is never the binding constraint.
	for i, node := range []string{"n1", "n2", "n3"} {
		if _, err := m.Begin(req(node)); err != nil {
			t.Fatalf("session %d on %s: %v", i, node, err)
		}
	}
	if _, err := m.Begin(req("n4")); !errors.Is(err, ErrClusterBusy) {
		t.Fatalf("err = %v, want ErrClusterBusy", err)
	}
}

// Limits are read per admission so an operator raising them does not have to
// restart the server.
func TestBeginReadsSettingsPerCall(t *testing.T) {
	cfg := store.DefaultTerminalSettings()
	cfg.MaxPerNode = 1
	cfg.MaxGlobal = 8
	f := newFake(cfg)
	m := New(f)

	if _, err := m.Begin(req("node-a")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Begin(req("node-a")); !errors.Is(err, ErrNodeBusy) {
		t.Fatalf("err = %v, want ErrNodeBusy", err)
	}

	cfg.MaxPerNode = 3
	f.setSettings(cfg)
	if _, err := m.Begin(req("node-a")); err != nil {
		t.Errorf("after raising the limit: %v", err)
	}
}

// A failed audit write must not leak the reserved slot, or repeated failures
// would wedge the node at its limit with nothing running.
func TestBeginReleasesSlotWhenAuditWriteFails(t *testing.T) {
	cfg := store.DefaultTerminalSettings()
	cfg.MaxPerNode = 1
	f := newFake(cfg)
	f.createErr = errors.New("disk full")
	m := New(f)

	if _, err := m.Begin(req("node-a")); err == nil {
		t.Fatal("expected an error")
	}
	if total, node := m.Active("node-a"); total != 0 || node != 0 {
		t.Errorf("active = (%d,%d), want (0,0)", total, node)
	}

	f.mu.Lock()
	f.createErr = nil
	f.mu.Unlock()
	if _, err := m.Begin(req("node-a")); err != nil {
		t.Errorf("after recovery: %v", err)
	}
}

func TestBeginRecordsAuditMetadata(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f)

	s, err := m.Begin(Request{
		UserID: 7, Username: "alice", NodeUUID: "node-x",
		Target: "container", Ref: "abc123", ClientIP: "192.168.1.5",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.created) != 1 {
		t.Fatalf("created %d records, want 1", len(f.created))
	}
	rec := f.created[0]
	if rec.UserID != 7 || rec.Username != "alice" || rec.NodeUUID != "node-x" ||
		rec.Target != "container" || rec.Ref != "abc123" || rec.ClientIP != "192.168.1.5" {
		t.Errorf("audit record = %+v", rec)
	}
	if s.Grace != store.DefaultTerminalSettings().Grace() {
		t.Errorf("grace = %v, want %v", s.Grace, store.DefaultTerminalSettings().Grace())
	}
	_ = s.Close(ReasonShellExited)
}

// Byte counters are the one thing this audit trail keeps about volume: they can
// surface "someone moved 40 GB off a node" without recording any content.
func TestSessionByteAccounting(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f)
	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatal(err)
	}

	s.AddIn(10)
	s.AddIn(5)
	s.AddOut(1000)
	s.AddOut(24)
	if in, out := s.Bytes(); in != 15 || out != 1024 {
		t.Errorf("bytes = (%d,%d), want (15,1024)", in, out)
	}

	if err := s.Close(ReasonShellExited); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 {
		t.Fatalf("closed %d records, want 1", len(f.closed))
	}
	got := f.closed[0]
	if got.BytesIn != 15 || got.BytesOut != 1024 || got.Reason != ReasonShellExited {
		t.Errorf("closed record = %+v", got)
	}
}

func TestSessionCloseIsIdempotent(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f)
	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Close(ReasonClientClosed); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ReasonIdleTimeout); err != nil {
		t.Fatal(err)
	}
	if f.closedCount() != 1 {
		t.Errorf("closed %d times, want 1", f.closedCount())
	}
	// The slot must be released exactly once, not decremented twice.
	if total, node := m.Active("node-a"); total != 0 || node != 0 {
		t.Errorf("active = (%d,%d), want (0,0)", total, node)
	}
}

// An unattended root shell must not stay open indefinitely.
func TestSessionIdleTimeoutFires(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f, WithIdleTimeout(100*time.Millisecond))

	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Expired():
	case <-time.After(3 * time.Second):
		t.Fatal("idle timeout did not fire")
	}
	_ = s.Close(ReasonIdleTimeout)
}

// Input must postpone the timeout; otherwise an operator typing steadily would
// still be disconnected.
func TestSessionTouchPostponesIdleTimeout(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f, WithIdleTimeout(300*time.Millisecond))
	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatal(err)
	}

	// Keep typing for longer than the window.
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		s.Touch()
		time.Sleep(40 * time.Millisecond)
		select {
		case <-s.Expired():
			t.Fatal("session expired while input was still arriving")
		default:
		}
	}

	// Stop typing and it should expire.
	select {
	case <-s.Expired():
	case <-time.After(3 * time.Second):
		t.Fatal("idle timeout did not fire after input stopped")
	}
	_ = s.Close(ReasonIdleTimeout)
}

// Output alone must not keep a session alive: `watch nvidia-smi` would otherwise
// hold a root shell open indefinitely.
func TestSessionOutputDoesNotPostponeIdleTimeout(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f, WithIdleTimeout(200*time.Millisecond))
	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				s.AddOut(4096) // chatty command, no operator input
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()

	select {
	case <-s.Expired():
	case <-time.After(3 * time.Second):
		t.Fatal("idle timeout did not fire despite no input")
	}
	_ = s.Close(ReasonIdleTimeout)
}

// Closing must stop the watchdog rather than leaving a goroutine per session.
func TestSessionCloseStopsIdleWatchdog(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f)
	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ReasonClientClosed); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Expired():
	case <-time.After(time.Second):
		t.Error("Expired was not closed by Close; the watchdog would leak")
	}
}

// Concurrent admissions must not exceed the limit: the reservation happens
// before the audit write precisely so two racing requests cannot both pass.
func TestBeginConcurrentAdmissionRespectsLimit(t *testing.T) {
	cfg := store.DefaultTerminalSettings()
	cfg.MaxPerNode = 4
	cfg.MaxGlobal = 4
	f := newFake(cfg)
	m := New(f)

	const attempts = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	var admitted []*Session
	rejected := 0

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.Begin(req("node-a"))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				rejected++
				return
			}
			admitted = append(admitted, s)
		}()
	}
	wg.Wait()

	if len(admitted) != cfg.MaxPerNode {
		t.Errorf("admitted %d sessions, want %d", len(admitted), cfg.MaxPerNode)
	}
	if rejected != attempts-cfg.MaxPerNode {
		t.Errorf("rejected %d, want %d", rejected, attempts-cfg.MaxPerNode)
	}
	if total, _ := m.Active("node-a"); total != cfg.MaxPerNode {
		t.Errorf("active total = %d, want %d", total, cfg.MaxPerNode)
	}

	for _, s := range admitted {
		_ = s.Close(ReasonClientClosed)
	}
	if total, node := m.Active("node-a"); total != 0 || node != 0 {
		t.Errorf("after closing all: active = (%d,%d), want (0,0)", total, node)
	}
}

// Releasing must not leave a map entry per node seen, or a long-lived server
// accumulates one for every node an operator ever opened a terminal on.
func TestReleaseRemovesEmptyNodeEntries(t *testing.T) {
	f := newFake(store.DefaultTerminalSettings())
	m := New(f)
	s, err := m.Begin(req("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close(ReasonClientClosed)

	m.mu.Lock()
	n := len(m.perNode)
	m.mu.Unlock()
	if n != 0 {
		t.Errorf("perNode retained %d entries, want 0", n)
	}
}
