//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

const enrollmentShutdownTimeout = 5 * time.Second

// enrollmentWriteTimeout is the listener's absolute bound on writing one
// answer. A handler whose answer takes longer (a diagnostics part) sets its
// own deadline. Tests shorten it.
var enrollmentWriteTimeout = 15 * time.Second

type enrollmentServer struct {
	mu               sync.Mutex
	addr             string
	handler          *enrollmentHandlerGate
	config           *tls.Config
	log              *slog.Logger
	run              *enrollmentServerRun
	materialReceiver *materialReceiver
	materialRequests *materialRequestTracker
}

type enrollmentServerRun struct {
	server   *http.Server
	listener net.Listener
	served   chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	serveErr error
	stopErr  error
}

func newEnrollmentServer(addr string, handler http.Handler, config *tls.Config, log *slog.Logger) (*enrollmentServer, error) {
	if strings.TrimSpace(addr) == "" || handler == nil {
		return nil, errors.New("enrollment HTTPS requires an address and handler")
	}
	if config == nil || len(config.Certificates) == 0 {
		return nil, errors.New("enrollment HTTPS requires a server certificate")
	}
	config = config.Clone()
	if config.MinVersion == 0 {
		config.MinVersion = tls.VersionTLS12
	}
	if config.MinVersion < tls.VersionTLS12 || config.MinVersion > tls.VersionTLS13 || (config.MaxVersion != 0 && config.MaxVersion < config.MinVersion) {
		return nil, errors.New("enrollment HTTPS requires valid TLS 1.2 or later versions")
	}
	for _, certificate := range config.Certificates {
		if len(certificate.Certificate) == 0 {
			return nil, errors.New("enrollment HTTPS certificate is empty")
		}
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return nil, errors.New("enrollment HTTPS certificate is invalid")
		}
		key, ok := certificate.PrivateKey.(crypto.Signer)
		if !ok || key == nil {
			return nil, errors.New("enrollment HTTPS requires a signing private key")
		}
		value := reflect.ValueOf(key)
		if (value.Kind() == reflect.Pointer || value.Kind() == reflect.Slice) && value.IsNil() {
			return nil, errors.New("enrollment HTTPS signing private key is nil")
		}
		if private, ok := key.(ed25519.PrivateKey); ok && len(private) != ed25519.PrivateKeySize {
			return nil, errors.New("enrollment HTTPS signing private key is invalid")
		}
		public, err := x509.MarshalPKIXPublicKey(key.Public())
		if err != nil || !bytes.Equal(public, leaf.RawSubjectPublicKeyInfo) {
			return nil, errors.New("enrollment HTTPS certificate and private key differ")
		}
		if now := time.Now(); now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
			return nil, errors.New("enrollment HTTPS certificate is not currently valid")
		}
	}
	if log == nil {
		log = slog.Default()
	}
	return &enrollmentServer{addr: addr, handler: newEnrollmentHandlerGate(handler), config: config, log: log}, nil
}

// adoptPreparedHandler reuses this server's running TLS listener. The prepared
// server is only a source of routes and must never have been started. A changed
// listener address or TLS identity needs a separate restart, not a live swap.
func (s *enrollmentServer) adoptPreparedHandler(prepared *enrollmentServer) error {
	if s == nil || prepared == nil || s == prepared {
		return errors.New("enrollment handler transfer requires distinct servers")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.run != nil || s.addr != prepared.addr || !sameEnrollmentTLS(s.config, prepared.config) {
		return errors.New("prepared enrollment handler does not match the active listener")
	}
	if s.materialRequests != prepared.materialRequests {
		return errors.New("prepared enrollment handler has a different material work owner")
	}
	if s.materialReceiver != prepared.materialReceiver {
		return errors.New("prepared enrollment handler has a different material receiver")
	}
	s.handler.install(prepared.handler.base)
	return nil
}

func (s *enrollmentServer) withdrawPreparedHandler() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handler.withdraw()
}

func (s *enrollmentServer) withdrawPreparedHandlerAndWait(ctx context.Context) (*enrollmentHandlerDrain, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handler.withdrawAndWait(ctx)
}

func sameEnrollmentTLS(a, b *tls.Config) bool {
	if a == nil || b == nil || a.MinVersion != b.MinVersion || a.MaxVersion != b.MaxVersion || a.ClientAuth != b.ClientAuth || a.SessionTicketsDisabled != b.SessionTicketsDisabled || len(a.Certificates) != len(b.Certificates) {
		return false
	}
	// Custom TLS hooks may close over different trust state even when their
	// function pointers match. A live transfer cannot prove them equivalent.
	if a.GetCertificate != nil || b.GetCertificate != nil || a.GetConfigForClient != nil || b.GetConfigForClient != nil || a.VerifyPeerCertificate != nil || b.VerifyPeerCertificate != nil || a.VerifyConnection != nil || b.VerifyConnection != nil {
		return false
	}
	if !slices.Equal(a.CipherSuites, b.CipherSuites) || !slices.Equal(a.CurvePreferences, b.CurvePreferences) || !slices.Equal(a.NextProtos, b.NextProtos) {
		return false
	}
	for i := range a.Certificates {
		if len(a.Certificates[i].Certificate) != len(b.Certificates[i].Certificate) {
			return false
		}
		for j := range a.Certificates[i].Certificate {
			if !bytes.Equal(a.Certificates[i].Certificate[j], b.Certificates[i].Certificate[j]) {
				return false
			}
		}
	}
	if (a.ClientCAs == nil) != (b.ClientCAs == nil) {
		return false
	}
	return a.ClientCAs == nil || a.ClientCAs.Equal(b.ClientCAs)
}

func (s *enrollmentServer) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.run != nil {
		select {
		case <-s.run.done:
		default:
			return errors.New("enrollment HTTPS server is already running or stopping")
		}
	}
	if s.materialRequests != nil {
		if err := s.materialRequests.reopen(); err != nil {
			return err
		}
	}
	s.handler.withdraw()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		return err
	}
	run := &enrollmentServerRun{
		listener: tls.NewListener(listener, s.config.Clone()),
		served:   make(chan struct{}), done: make(chan struct{}),
		server: &http.Server{
			Handler: s.handler, ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
			ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
			WriteTimeout: enrollmentWriteTimeout, IdleTimeout: 30 * time.Second,
		},
	}
	s.run = run
	go func() {
		run.serveErr = run.server.Serve(run.listener)
		if errors.Is(run.serveErr, http.ErrServerClosed) {
			run.serveErr = nil
		}
		close(run.served)
		if run.serveErr == nil {
			return
		}
		shutdown, cancel := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
		defer cancel()
		_ = run.close(shutdown)
	}()
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), enrollmentShutdownTimeout)
			defer cancel()
			_ = run.close(shutdown)
		case <-run.done:
		}
	}()
	return nil
}

func (s *enrollmentServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run == nil {
		return s.addr
	}
	return s.run.listener.Addr().String()
}

func (s *enrollmentServer) Wait() error {
	s.mu.Lock()
	run := s.run
	s.mu.Unlock()
	if run == nil {
		return errors.New("enrollment HTTPS server has not started")
	}
	<-run.done
	return run.stopErr
}

// Close waits for graceful shutdown. On timeout it force-closes connections;
// callers must also Wait before restarting after an unsuccessful Close.
func (s *enrollmentServer) Close(ctx context.Context) error {
	s.mu.Lock()
	s.handler.withdraw()
	run := s.run
	requests := s.materialRequests
	if requests != nil {
		requests.closeAdmissions()
	}
	s.mu.Unlock()
	var err error
	if run == nil {
		if requests != nil {
			return requests.wait(ctx)
		}
		return nil
	}
	err = run.close(ctx)
	if requests != nil {
		waitErr := requests.wait(ctx)
		if waitErr == nil && ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			select {
			case <-run.done:
				// A prior forced close can retain its old timeout result. A
				// later call may release its owner only after Serve stopped and
				// every material handler has actually returned.
				return nil
			default:
			}
		}
		err = errors.Join(err, waitErr)
	}
	return err
}

func (r *enrollmentServerRun) close(ctx context.Context) error {
	select {
	case <-r.done:
		return r.stopErr
	default:
	}
	r.stopOnce.Do(func() {
		go func() {
			r.stopErr = r.server.Shutdown(ctx)
			if r.stopErr != nil {
				r.stopErr = errors.Join(r.stopErr, r.server.Close())
			}
			// Shutdown may win the race before Serve registers its listener.
			r.stopErr = errors.Join(r.stopErr, r.closeListener())
			<-r.served
			r.stopErr = errors.Join(r.stopErr, r.serveErr)
			close(r.done)
		}()
	})
	select {
	case <-r.done:
		return r.stopErr
	case <-ctx.Done():
		return errors.Join(ctx.Err(), r.server.Close(), r.closeListener())
	}
}

func (r *enrollmentServerRun) closeListener() error {
	err := r.listener.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
