// Package traceexport sends the gateway's per-request records (uxrecord.Request)
// to an OpenTelemetry collector as OTLP/HTTP JSON spans: one SERVER span per
// forwarded request, built after the reply ended from the record alone, so the
// span says exactly what the request view says (ADR 0046). It is a leaf: the
// app hands it records and its configuration; it knows nothing of the store.
//
// Observe never blocks the gateway. A bounded queue feeds one goroutine that
// batches, encodes and posts; a full queue drops the record and counts it, a
// collector that answers 429/502/503/504 or cannot be reached is retried a few
// times with backoff, anything else is dropped and reported in Status.
package traceexport

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Config is where the spans go. It is the resolved form of the platform's
// telemetry export setting: header values are in the clear here.
type Config struct {
	Enabled bool
	// Endpoint is the collector's OTLP/HTTP address: a base URL
	// (http://collector:4318), to which /v1/traces is added, or the full
	// traces URL.
	Endpoint string
	// Headers go with every export request (an authorization token, a
	// tenant id).
	Headers map[string]string
	// SampleRatio is the share of the traces the gateway starts that are
	// sampled, 0..1; a request continuing a caller's trace keeps the
	// caller's decision.
	SampleRatio float64
}

// reservedHeaders are set by the exporter itself.
var reservedHeaders = map[string]bool{"content-type": true, "content-encoding": true, "content-length": true, "host": true}

// TracesURL validates an endpoint and returns the URL spans are posted to.
func TracesURL(endpoint string) (string, error) {
	raw := strings.TrimSpace(endpoint)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("endpoint must be an http(s) URL without credentials, query or fragment, like http://collector:4318")
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, "/v1/traces") {
		path += "/v1/traces"
	}
	u.Path = path
	return u.String(), nil
}

// ConfigError says which part of a configuration is wrong.
type ConfigError struct {
	// Field is "endpoint", "header" or "sample_ratio".
	Field string
	// Header names the header at fault (Field "header").
	Header string
	Msg    string
}

func (e *ConfigError) Error() string { return e.Msg }

// Validate reports what is wrong with a configuration (a *ConfigError). A
// disabled one may keep a half-written endpoint; its headers and ratio must
// still be valid, since they are stored.
func (c Config) Validate() error {
	if c.SampleRatio < 0 || c.SampleRatio > 1 {
		return &ConfigError{Field: "sample_ratio", Msg: "sample ratio must be between 0 and 1"}
	}
	for name, value := range c.Headers {
		switch {
		case !validHeaderName(name):
			return &ConfigError{Field: "header", Header: name, Msg: fmt.Sprintf("header name %q is not a valid HTTP header name", name)}
		case reservedHeaders[strings.ToLower(name)]:
			return &ConfigError{Field: "header", Header: name, Msg: fmt.Sprintf("header %q is set by the exporter", name)}
		case strings.ContainsAny(value, "\r\n\x00"):
			return &ConfigError{Field: "header", Header: name, Msg: fmt.Sprintf("header %q has a line break in its value", name)}
		}
	}
	if !c.Enabled {
		return nil
	}
	if _, err := TracesURL(c.Endpoint); err != nil {
		return &ConfigError{Field: "endpoint", Msg: err.Error()}
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// Resource describes this control plane on every span.
type Resource struct {
	// ServiceVersion is the server's build version.
	ServiceVersion string
	// InstanceID and Name are the control plane's identity (the setting
	// other control planes see on shared storages).
	InstanceID, Name string
}

// Options configures an Exporter; zero fields take the defaults.
type Options struct {
	Resource Resource
	// Client posts the batches; its timeout bounds one attempt.
	Client *http.Client
	// AllowPrivateHosts permits only administrator-approved private HTTP sources.
	AllowPrivateHosts []string
	// Queue is the capacity of the Observe queue.
	Queue int
	// BatchSize and FlushEvery shape the posts: a batch goes when it is
	// full or FlushEvery after its first span.
	BatchSize  int
	FlushEvery time.Duration
	// Retries is how many times a retryable failure is tried again;
	// Backoff the first wait, doubled each time unless the collector says
	// Retry-After.
	Retries int
	Backoff time.Duration
	// MaxRetryAfter caps a collector's Retry-After.
	MaxRetryAfter time.Duration
	// CloseWait bounds how long Close keeps sending what is queued.
	CloseWait time.Duration
	// Now is the clock; tests inject one.
	Now func() time.Time
}

// Defaults for the zero fields of Options.
const (
	DefaultQueue         = 8192
	DefaultBatchSize     = 512
	DefaultFlushEvery    = time.Second
	DefaultRetries       = 3
	DefaultBackoff       = time.Second
	DefaultMaxRetryAfter = 30 * time.Second
	DefaultCloseWait     = 5 * time.Second
	defaultTimeout       = 10 * time.Second
)

func (o Options) withDefaults() Options {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: defaultTimeout, Transport: outbound.Transport(func() (proto.NetworkConfig, error) { return proto.NetworkConfig{}, nil }, o.AllowPrivateHosts...)}
	}
	if o.Queue <= 0 {
		o.Queue = DefaultQueue
	}
	if o.BatchSize <= 0 {
		o.BatchSize = DefaultBatchSize
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = DefaultFlushEvery
	}
	if o.Retries < 0 {
		o.Retries = 0
	} else if o.Retries == 0 {
		o.Retries = DefaultRetries
	}
	if o.Backoff <= 0 {
		o.Backoff = DefaultBackoff
	}
	if o.MaxRetryAfter <= 0 {
		o.MaxRetryAfter = DefaultMaxRetryAfter
	}
	if o.CloseWait <= 0 {
		o.CloseWait = DefaultCloseWait
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// active is a configuration ready to use.
type active struct {
	cfg Config
	url string
}

// Exporter exports request records as spans.
type Exporter struct {
	opts   Options
	cfg    atomic.Pointer[active] // nil: disabled
	queue  chan uxrecord.Request
	stop   chan struct{}
	done   chan struct{}
	closed atomic.Bool
	once   sync.Once
	// ctx bounds every post; Close cancels it CloseWait after it began, so
	// a collector that hangs cannot hold the shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	exported, dropped, failed atomic.Uint64
	mu                        sync.Mutex
	lastError                 string
	lastErrorAt, lastExportAt time.Time
}

// New builds an Exporter, disabled until Configure enables it, and starts its
// sender.
func New(opts Options) *Exporter {
	opts = opts.withDefaults()
	e := &Exporter{opts: opts, queue: make(chan uxrecord.Request, opts.Queue), stop: make(chan struct{}), done: make(chan struct{})}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	go e.run()
	return e
}

// Configure switches the export to cfg at once: the next batch goes to the
// new endpoint, and a disabled configuration stops both sampling and
// sending. An invalid enabled configuration is refused and the old one kept.
func (e *Exporter) Configure(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !cfg.Enabled {
		e.cfg.Store(nil)
		return nil
	}
	u, _ := TracesURL(cfg.Endpoint)
	headers := make(map[string]string, len(cfg.Headers))
	for k, v := range cfg.Headers {
		headers[k] = v
	}
	cfg.Headers = headers
	e.cfg.Store(&active{cfg: cfg, url: u})
	return nil
}

// Sample is the gateway's decision for a trace it starts: sampled at the
// configured ratio while export is on, never while it is off.
func (e *Exporter) Sample(t tracecontext.TraceID) bool {
	a := e.cfg.Load()
	return a != nil && tracecontext.SampledByRatio(t, a.cfg.SampleRatio)
}

// Observe queues a finished request's span when export is on and the
// request's trace is sampled. It never blocks: a full queue drops the record
// and counts it.
func (e *Exporter) Observe(r uxrecord.Request) {
	if e.cfg.Load() == nil || !r.Trace.Sampled() || r.Trace.Trace.IsZero() || r.Trace.ID.IsZero() {
		return
	}
	if e.closed.Load() {
		e.dropped.Add(1)
		return
	}
	select {
	case e.queue <- r:
	default:
		e.dropped.Add(1)
	}
}

// Status is what the export did since the server started.
type Status struct {
	Enabled     bool
	Endpoint    string // the traces URL in use; empty when off
	SampleRatio float64
	// Exported counts spans the collector accepted; Dropped those lost
	// before sending (a full queue); Failed those a collector refused or
	// that could not be delivered.
	Exported, Dropped, Failed uint64
	Queued                    int
	LastError                 string
	LastErrorAt, LastExportAt time.Time
}

// Status reports the export's state and counters.
func (e *Exporter) Status() Status {
	st := Status{Exported: e.exported.Load(), Dropped: e.dropped.Load(), Failed: e.failed.Load(), Queued: len(e.queue)}
	if a := e.cfg.Load(); a != nil {
		st.Enabled, st.Endpoint, st.SampleRatio = true, a.url, a.cfg.SampleRatio
	}
	e.mu.Lock()
	st.LastError, st.LastErrorAt, st.LastExportAt = e.lastError, e.lastErrorAt, e.lastExportAt
	e.mu.Unlock()
	return st
}

// Close stops taking records, sends what is queued within CloseWait and
// stops the sender. Records observed during or after Close are dropped and
// counted. Close is idempotent.
func (e *Exporter) Close() {
	e.once.Do(func() {
		e.closed.Store(true)
		close(e.stop)
		deadline := time.AfterFunc(e.opts.CloseWait, e.cancel)
		<-e.done
		deadline.Stop()
		e.cancel()
		// Records that raced in after the sender's last drain.
		for len(e.queue) > 0 {
			<-e.queue
			e.dropped.Add(1)
		}
	})
}

// run is the sender: it batches the queue and posts each batch with the
// configuration of the moment. On stop it sends what is queued, as long as
// Close's deadline allows, and counts the rest as dropped.
func (e *Exporter) run() {
	defer close(e.done)
	batch := make([]uxrecord.Request, 0, e.opts.BatchSize)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	send := func() {
		if len(batch) > 0 && e.ctx.Err() == nil {
			e.post(e.ctx, batch)
		} else {
			e.dropped.Add(uint64(len(batch)))
		}
		batch = batch[:0]
		timer.Stop()
	}
	for {
		select {
		case r := <-e.queue:
			if len(batch) == 0 {
				timer.Reset(e.opts.FlushEvery)
			}
			batch = append(batch, r)
			if len(batch) >= e.opts.BatchSize {
				send()
			}
		case <-timer.C:
			send()
		case <-e.stop:
		drain:
			for {
				select {
				case r := <-e.queue:
					batch = append(batch, r)
					if len(batch) >= e.opts.BatchSize {
						send()
					}
				default:
					break drain
				}
			}
			send()
			return
		}
	}
}

func (e *Exporter) noteError(msg string) {
	e.mu.Lock()
	e.lastError, e.lastErrorAt = msg, e.opts.Now()
	e.mu.Unlock()
}

func (e *Exporter) noteExport() {
	e.mu.Lock()
	e.lastExportAt = e.opts.Now()
	e.mu.Unlock()
}
