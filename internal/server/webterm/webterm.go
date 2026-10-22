// Package webterm owns web terminal session policy on the server: the kill
// switch, concurrency limits, idle timeouts and audit records.
//
// It is deliberately separate from the HTTP handler. Admission is where this
// feature's safety lives, and keeping it here means the rules can be tested
// as plain function calls instead of by standing up a server and counting
// status codes. The handler is left with transport concerns only.
//
// The agent has no say in any of this: it does not know who the operator is,
// and it has no access to platform settings. Its only policy input is the
// SIGTERM-to-SIGKILL grace period, which the server passes in the stream header.
package webterm

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Close reasons recorded in the audit trail.
const (
	ReasonClientClosed = "client_closed"
	ReasonShellExited  = "shell_exited"
	ReasonIdleTimeout  = "idle_timeout"
	ReasonNodeGone     = "node_disconnected"
	ReasonServerError  = "server_error"
)

// Admission failures. Callers map these to status codes; the package does not
// import net/http so it stays testable as pure policy.
var (
	// ErrDisabled means an operator turned the feature off.
	ErrDisabled = errors.New("web terminal is disabled in platform settings")
	// ErrNodeBusy means this node is at its concurrent session limit.
	ErrNodeBusy = errors.New("too many terminal sessions on this node")
	// ErrClusterBusy means the platform-wide limit is reached.
	ErrClusterBusy = errors.New("too many terminal sessions on this platform")
)

// Sessions is the slice of the store this package needs. Narrow by design: it
// keeps the fake in tests small and makes the dependency obvious.
type Sessions interface {
	TerminalSettings() store.TerminalSettings
	CreateTerminalSession(*store.TerminalSession) (*store.TerminalSession, error)
	CloseTerminalSession(id uint, bytesIn, bytesOut int64, reason string) error
}

// Request describes a session an operator is asking for.
type Request struct {
	UserID   uint
	Username string
	NodeUUID string
	Target   string // store's host | container
	Ref      string // container id when Target is container
	ClientIP string
}

// Manager admits sessions and tracks the live ones.
type Manager struct {
	sessions Sessions
	now      func() time.Time // injectable for tests
	idleOver time.Duration    // see WithIdleTimeout

	mu      sync.Mutex
	perNode map[string]int
	total   int
}

// Option configures a Manager.
type Option func(*Manager)

// WithIdleTimeout overrides the idle window that platform settings would supply.
//
// Settings express the timeout in whole minutes, which is the right unit for an
// operator but far too coarse for a test to wait on. This is the seam that lets
// idle behaviour be tested in milliseconds; production leaves it unset and the
// configured value applies.
func WithIdleTimeout(d time.Duration) Option {
	return func(m *Manager) { m.idleOver = d }
}

// WithClock replaces the time source used to stamp activity.
func WithClock(now func() time.Time) Option {
	return func(m *Manager) { m.now = now }
}

// New returns a Manager backed by s.
func New(s Sessions, opts ...Option) *Manager {
	m := &Manager{sessions: s, now: time.Now, perNode: map[string]int{}}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Begin admits a session, writing its audit record.
//
// Limits are checked here rather than on the agent because only the server sees
// the whole cluster. They exist mainly to contain accidents: a UI reconnect loop
// can otherwise exhaust a node's ptys and the agent's goroutines within seconds.
//
// Settings are read per call, so a limit change takes effect immediately.
func (m *Manager) Begin(req Request) (*Session, error) {
	cfg := m.sessions.TerminalSettings()
	if !cfg.Enabled {
		return nil, ErrDisabled
	}

	m.mu.Lock()
	if m.total >= cfg.MaxGlobal {
		m.mu.Unlock()
		return nil, ErrClusterBusy
	}
	if m.perNode[req.NodeUUID] >= cfg.MaxPerNode {
		m.mu.Unlock()
		return nil, ErrNodeBusy
	}
	// Reserve the slot before the (slower) database write so two concurrent
	// admissions cannot both pass the check.
	m.perNode[req.NodeUUID]++
	m.total++
	m.mu.Unlock()

	rec, err := m.sessions.CreateTerminalSession(&store.TerminalSession{
		UserID:   req.UserID,
		Username: req.Username,
		NodeUUID: req.NodeUUID,
		Target:   req.Target,
		Ref:      req.Ref,
		ClientIP: req.ClientIP,
		// StartedAt is filled in by the store.
	})
	if err != nil {
		m.release(req.NodeUUID)
		return nil, err
	}

	idle := cfg.IdleTimeout()
	if m.idleOver > 0 {
		idle = m.idleOver
	}
	s := &Session{
		ID:       rec.ID,
		NodeUUID: req.NodeUUID,
		Grace:    cfg.Grace(),
		mgr:      m,
		idle:     idle,
		expired:  make(chan struct{}),
	}
	s.touch(m.now())
	if s.idle > 0 {
		go s.watchIdle()
	}
	return s, nil
}

func (m *Manager) release(nodeUUID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := m.perNode[nodeUUID]; n <= 1 {
		delete(m.perNode, nodeUUID)
	} else {
		m.perNode[nodeUUID] = n - 1
	}
	if m.total > 0 {
		m.total--
	}
}

// Active reports the number of live sessions overall and on one node.
func (m *Manager) Active(nodeUUID string) (total, node int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total, m.perNode[nodeUUID]
}

// Session is one live terminal session.
type Session struct {
	ID       uint
	NodeUUID string
	// Grace is the SIGTERM-to-SIGKILL window the agent should use; it travels in
	// the stream header so the agent needs no settings of its own.
	Grace time.Duration

	mgr  *Manager
	idle time.Duration

	lastActivity atomic.Int64 // unix nanoseconds
	bytesIn      atomic.Int64
	bytesOut     atomic.Int64

	expiredOnce sync.Once
	expired     chan struct{}

	closeOnce sync.Once
	closeErr  error
}

func (s *Session) touch(t time.Time) { s.lastActivity.Store(t.UnixNano()) }

// Touch records operator input.
//
// Only input counts. Keying the timeout on output instead would let any
// long-running command hold a root shell open forever, and `watch nvidia-smi`
// is exactly the sort of thing people leave running on a GPU node.
func (s *Session) Touch() { s.touch(s.mgr.now()) }

// AddIn counts bytes sent to the pty; AddOut counts bytes received from it.
// Both are called once per frame, so accounting never touches a per-byte path.
func (s *Session) AddIn(n int)  { s.bytesIn.Add(int64(n)) }
func (s *Session) AddOut(n int) { s.bytesOut.Add(int64(n)) }

// Bytes returns the transfer totals recorded so far.
func (s *Session) Bytes() (in, out int64) { return s.bytesIn.Load(), s.bytesOut.Load() }

// Expired is closed when the idle timeout fires. The handler selects on it and
// tears the session down with ReasonIdleTimeout.
func (s *Session) Expired() <-chan struct{} { return s.expired }

// watchIdle closes Expired once input has been absent for the idle window.
//
// The check interval is a fraction of the window so a session is never held
// much past its deadline, and is floored so a very short test timeout does not
// spin.
func (s *Session) watchIdle() {
	interval := s.idle / 10
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.expired:
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, s.lastActivity.Load())) >= s.idle {
				s.expiredOnce.Do(func() { close(s.expired) })
				return
			}
		}
	}
}

// Close finalises the audit record and frees the session's slot. Safe to call
// more than once; only the first call is recorded.
func (s *Session) Close(reason string) error {
	s.closeOnce.Do(func() {
		s.expiredOnce.Do(func() { close(s.expired) }) // stop the idle watchdog
		in, out := s.Bytes()
		s.closeErr = s.mgr.sessions.CloseTerminalSession(s.ID, in, out, reason)
		s.mgr.release(s.NodeUUID)
	})
	return s.closeErr
}
