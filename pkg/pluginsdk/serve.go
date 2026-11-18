package pluginsdk

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// serveStreams bounds the server's streams on the serve connection: both
// call pools at the manifest's largest runtime.max_concurrency (1024),
// plus room for lifecycle calls, which take no slot. The default (250)
// would make a full pair of pools wait for a stream instead of being
// refused at once.
const serveStreams = 2*1024 + 32

// startStopWait bounds how long shutdown waits for Start to return,
// drainWait how long handlers in flight get to finish when the plugin ends
// on its own, and goodbyeWait the HTTP/2 goodbye after them.
const (
	startStopWait = 5 * time.Second
	drainWait     = 2 * time.Second
	goodbyeWait   = 100 * time.Millisecond
)

// session is one run of the plugin on its two connections.
type session struct {
	p         *Plugin
	runCtx    context.Context
	shutdown  chan struct{}
	stopOnce  sync.Once
	initMu    sync.Mutex
	started   atomic.Bool
	inflight  atomic.Int64
	startDone chan error
	failed    chan error
	gone      chan struct{}
	goneOnce  sync.Once
}

func serveConns(ctx context.Context, cfg Config, serve, host net.Conn, parentPoll time.Duration) error {
	if err := cfg.validate(); err != nil {
		_ = serve.Close()
		_ = host.Close()
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	client, closeClient, err := hostClient(ctx, host)
	if err != nil {
		_ = serve.Close()
		return err
	}
	defer func() { _ = closeClient() }()
	p := &Plugin{cfg: cfg, log: logger(cfg), client: client,
		held: map[string]extapi.HeldState{}, own: map[string]uint64{}}
	s := &session{p: p, runCtx: ctx, shutdown: make(chan struct{}),
		startDone: make(chan error, 1), failed: make(chan error, 1), gone: make(chan struct{})}

	ln := newOneConnListener(serve)
	srv := &http.Server{
		Handler:   http.HandlerFunc(s.route),
		Protocols: h2c(),
		HTTP2:     &http.HTTP2Config{MaxConcurrentStreams: serveStreams},
		ConnState: func(_ net.Conn, st http.ConnState) {
			if st == http.StateClosed || st == http.StateHijacked {
				s.goneOnce.Do(func() { close(s.gone) })
			}
		},
		ReadHeaderTimeout: 30 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	drain := false
	defer func() {
		// When the plugin ends on its own (shutdown, a failed init or
		// Start), let the answers in flight reach the server first: wait
		// for the handlers to return, then say goodbye briefly. A server
		// that hung up already ends this at once.
		if drain {
			deadline := time.Now().Add(drainWait)
			for s.inflight.Load() > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			sctx, scancel := context.WithTimeout(context.Background(), goodbyeWait)
			_ = srv.Shutdown(sctx)
			scancel()
		}
		_ = ln.Close()
		_ = srv.Close()
		<-served
	}()

	var parentGone <-chan struct{}
	if parentPoll > 0 {
		parentGone = watchParent(ctx, parentPoll)
	}
	startDone := s.startDone
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.gone:
			return ErrServerGone
		case <-parentGone:
			return ErrServerGone
		case err := <-s.failed:
			drain = true
			return err
		case err := <-startDone:
			if err != nil {
				drain = true
				return err
			}
			startDone = nil // Start finished its work; keep serving
		case <-s.shutdown:
			drain = true
			cancel()
			if startDone != nil && s.started.Load() {
				select {
				case err := <-startDone:
					if err != nil && !errors.Is(err, context.Canceled) {
						return err
					}
				case <-time.After(startStopWait):
				}
			}
			return nil
		}
	}
}

func (s *session) route(w http.ResponseWriter, r *http.Request) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	r, cancel := withDeadline(r)
	defer cancel()
	switch r.URL.Path {
	case extapi.PathInitialize:
		s.initialize(w, r)
	case extapi.PathHealth:
		s.health(w, r)
	case extapi.PathShutdown:
		if r.Method != http.MethodPost {
			writeError(w, methodRefusal(r))
			return
		}
		w.WriteHeader(http.StatusNoContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		s.stopOnce.Do(func() { close(s.shutdown) })
	default:
		if !s.p.ready.Load() {
			writeError(w, Refuse(extapi.ReasonFailedPrecondition, extapi.ErrorWireInvalid,
				"initialize has not been answered", extapi.Params{"object": "handshake_request", "field": "_root", "rule": extapi.RuleRequired}))
			return
		}
		if s.p.cfg.Handler == nil {
			writeError(w, Refuse(extapi.ReasonNotFound, extapi.ErrorWireInvalid,
				"the plugin serves no "+r.URL.Path, extapi.Params{"object": "request", "field": "path", "rule": extapi.RuleEnum}))
			return
		}
		s.p.cfg.Handler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), pluginKey{}, s.p)))
	}
}

func methodRefusal(r *http.Request) *Error {
	return Refuse(extapi.ReasonUnsupported, extapi.ErrorWireInvalid, r.Method+" is not served on "+r.URL.Path,
		extapi.Params{"object": "request", "field": "method", "rule": extapi.RuleEnum})
}

func (s *session) initialize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, methodRefusal(r))
		return
	}
	s.initMu.Lock() // one initialize at a time; the first accepted wins
	defer s.initMu.Unlock()
	p := s.p
	if p.ready.Load() {
		writeError(w, Refuse(extapi.ReasonConflict, extapi.ErrorWireInvalid, "initialize was already answered",
			extapi.Params{"object": "handshake_request", "field": "_root", "rule": extapi.RuleDuplicate}))
		return
	}
	raw, err := readBody(r.Body)
	if err != nil {
		writeError(w, refusalOf(err, extapi.ReasonInvalidArgument))
		return
	}
	req, err := extapi.DecodeHandshakeRequest(raw)
	if err != nil {
		reason := extapi.ReasonInvalidArgument
		if extapi.CodeOf(err) == extapi.ErrorProtocolUnsupported {
			reason = extapi.ReasonUnsupported
		}
		writeError(w, refusalOf(err, reason))
		return
	}
	if req.Plugin.ID != p.cfg.ID {
		writeError(w, Refuse(extapi.ReasonFailedPrecondition, extapi.ErrorWireInvalid,
			fmt.Sprintf("the server started package %s with the binary of %s", req.Plugin.ID, p.cfg.ID),
			extapi.Params{"object": "handshake_request", "field": "plugin.id", "rule": extapi.RuleRelation}))
		return
	}
	use, refusal := negotiate(req.Core.Offer, p.cfg.Requires, p.cfg.Uses)
	if refusal != nil {
		writeError(w, refusalOf(refusal, extapi.ReasonUnsupported))
		return
	}
	p.req, p.use = req, use
	for _, h := range req.Held {
		p.held[h.Kind] = h
	}
	if p.cfg.Init != nil {
		if err := p.cfg.Init(r.Context(), p); err != nil {
			var e *Error
			if errors.As(err, &e) {
				writeError(w, e)
				return
			}
			s.fail(fmt.Errorf("pluginsdk: init: %w", err))
			writeError(w, Refuse(extapi.ReasonInternal, extapi.ErrorWireInvalid, "the plugin failed to initialize",
				extapi.Params{"object": "handshake_request", "field": "_root", "rule": extapi.RuleRelation}))
			return
		}
	}
	resp := extapi.HandshakeResponse{Protocol: extapi.ProtocolMajor, ID: p.cfg.ID, Version: p.cfg.Version,
		ManifestSHA256: p.cfg.ManifestSHA256, Use: use, Services: p.cfg.Services}
	if len(resp.Use) == 0 {
		resp.Use = nil
	}
	p.ready.Store(true)
	writeJSON(w, resp)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	p.log.Debug("initialized", "use", compactJSON(use))
	if p.cfg.Start != nil {
		s.started.Store(true)
		go func() { s.startDone <- p.cfg.Start(s.runCtx, p) }()
	}
}

// fail ends the session with err, as a crash.
func (s *session) fail(err error) {
	select {
	case s.failed <- err:
	default:
	}
}

func (s *session) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, methodRefusal(r))
		return
	}
	h := extapi.Health{Status: extapi.HealthOK}
	if s.p.cfg.Health != nil {
		h = s.p.cfg.Health(r.Context())
	}
	if err := h.Validate(); err != nil {
		writeError(w, refusalOf(err, extapi.ReasonInternal))
		return
	}
	writeJSON(w, h)
}
