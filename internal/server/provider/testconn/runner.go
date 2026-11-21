package testconn

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// runTimeout bounds one whole run on top of the per-check timeouts.
const runTimeout = 2 * time.Minute

// ErrRunTimeout ends a run that did not finish within its overall deadline.
var ErrRunTimeout = errors.New("provider connection test exceeded its deadline")

// Runner executes checks in order. The clock, the resolver, the dialer, the
// proxy decision and the environment are injectable for hermetic tests.
type Runner struct {
	Now        func() time.Time
	LookupHost func(ctx context.Context, host string) ([]string, error)
	Dial       func(ctx context.Context, network, address string) (net.Conn, error)
	// Proxy decides whether a request to the endpoint goes through a proxy.
	Proxy func(*http.Request) (*url.URL, error)
	// Getenv reads the proxy variables proxy_detect names.
	Getenv func(string) string
	// TLSConfig is the base TLS client configuration; nil is the default one.
	TLSConfig *tls.Config
	// HTTPClient sends the clock-skew request. Nil builds a client from Proxy,
	// Dial and TLSConfig, so the request takes the same path as the checks
	// before it.
	HTTPClient *http.Client
	// Timeout bounds one whole run; zero is two minutes.
	Timeout time.Duration
}

// New returns a Runner with real network operations.
func New() *Runner {
	dialer := &net.Dialer{Timeout: networkTimeout}
	return &Runner{
		Now:        time.Now,
		LookupHost: net.DefaultResolver.LookupHost,
		Dial:       dialer.DialContext,
		Proxy:      http.ProxyFromEnvironment,
		Getenv:     os.Getenv,
	}
}

// runState is one run's progress. passable holds the checks whose dependents
// may run: the ones that passed and the ones skipped for a reason that blocks
// nothing.
type runState struct {
	r        *Runner
	parent   context.Context
	in       Input
	seq      int
	report   ReportFunc
	passable map[string]bool
	failed   []string
	stopped  error
}

// Run executes all checks and returns the summary. A failed check skips its
// dependents; independent checks continue. A cancelled ctx stops the run
// without reporting the check it interrupted and returns ctx's error; a run
// that outlives its deadline returns ErrRunTimeout.
func (r *Runner) Run(ctx context.Context, in Input, report ReportFunc) (Summary, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = runTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	st := &runState{r: r, parent: ctx, in: in, report: report, passable: map[string]bool{}}
	st.run(runCtx)
	if st.stopped != nil {
		return Summary{}, st.stopped
	}
	return Summary{OK: len(st.failed) == 0, Failed: st.failed}, nil
}

func (st *runState) run(ctx context.Context) {
	in := st.in
	if in.EndpointNotApplicable {
		st.skip(CheckEndpointFormat, HintNotApplicable, nil)
		st.step(ctx, CheckCredentialsShape, nil, func(context.Context) (map[string]any, error) {
			return checkCredentialsShape(in)
		})
		for _, name := range networkChecks {
			st.skip(name, HintNotApplicable, nil)
		}
		st.vendor(ctx)
		return
	}
	ok := st.step(ctx, CheckEndpointFormat, nil, func(context.Context) (map[string]any, error) {
		return checkEndpointFormat(in.Endpoint)
	})
	st.step(ctx, CheckCredentialsShape, nil, func(context.Context) (map[string]any, error) {
		return checkCredentialsShape(in)
	})
	if !ok {
		// Without a valid endpoint the network and vendor checks have nothing
		// to reach.
		detail := map[string]any{"depends": CheckEndpointFormat}
		for _, name := range networkChecks {
			st.skip(name, HintDependsFailed, detail)
		}
		for _, vc := range in.Vendor {
			st.skip(vc.Name, HintDependsFailed, detail)
		}
		return
	}
	// endpoint_format passed, so parsing again succeeds. The parsed URL stays
	// in the run; it is not part of any wire detail.
	u, _ := url.Parse(strings.TrimSpace(in.Endpoint))
	st.network(ctx, u)
	st.vendor(ctx)
}

func (st *runState) vendor(ctx context.Context) {
	for _, vc := range st.in.Vendor {
		st.step(ctx, vc.Name, vc.Depends, vc.Run)
	}
}

// halted reports whether the run is over: the caller went away, or the run's
// deadline passed.
func (st *runState) halted(ctx context.Context) bool {
	if st.stopped != nil {
		return true
	}
	if err := st.parent.Err(); err != nil {
		st.stopped = err
		return true
	}
	if ctx.Err() != nil {
		st.stopped = ErrRunTimeout
		return true
	}
	return false
}

func (st *runState) step(ctx context.Context, name string, depends []string, fn func(context.Context) (map[string]any, error)) bool {
	if st.halted(ctx) {
		return false
	}
	for _, d := range depends {
		if !st.passable[d] {
			st.skip(name, HintDependsFailed, map[string]any{"depends": d})
			return false
		}
	}
	started := st.r.Now()
	detail, err := fn(ctx)
	if st.parent.Err() != nil {
		// The caller went away mid-check: whatever the check returned is the
		// cancellation, not a finding about the provider.
		st.stopped = st.parent.Err()
		return false
	}
	var skipped *skipCheck
	if errors.As(err, &skipped) {
		st.skip(name, skipped.hint, nil)
		return false
	}
	c := Check{Name: name, Status: StatusOK, DurationMS: st.r.Now().Sub(started).Milliseconds(), Detail: detail}
	if err != nil {
		c.Status = StatusFailed
		c.Code = messages.ErrorProviderCheckFailed
		var f *Failure
		if errors.As(err, &f) {
			c.Code, c.Hint = f.Code, f.Hint
			if len(f.Detail) > 0 {
				c.Detail = f.Detail
			}
		}
		st.failed = append(st.failed, name)
		st.logFailure(c, err)
	}
	st.emit(c)
	st.passable[name] = c.Status == StatusOK
	return c.Status == StatusOK
}

// skipCheck is returned by a check that finds it has nothing to examine; the
// skip blocks nothing that depends on the check.
type skipCheck struct{ hint string }

func (s *skipCheck) Error() string { return "check skipped: " + s.hint }

// skip reports a check that did not run. Only a skip caused by a failed
// dependency blocks the checks that depend on this one.
func (st *runState) skip(name, hint string, detail map[string]any) {
	if st.stopped != nil {
		return
	}
	st.emit(Check{Name: name, Status: StatusSkipped, Hint: hint, Detail: detail})
	st.passable[name] = hint != HintDependsFailed
}

func (st *runState) emit(c Check) {
	st.seq++
	c.Seq = st.seq
	st.report(c)
}

// logFailure records what the wire leaves out: the cause of a failed check,
// with the credential values removed.
func (st *runState) logFailure(c Check, err error) {
	cause := err
	var f *Failure
	if errors.As(err, &f) && f.Err != nil {
		cause = f.Err
	}
	text := cause.Error()
	if st.in.Redact != nil {
		text = st.in.Redact(text)
	}
	slog.Default().Warn("provider connection check failed",
		"kind", st.in.Kind, "check", c.Name, "code", c.Code, "cause", text)
}

func checkEndpointFormat(raw string) (map[string]any, error) {
	trimmed := strings.TrimSpace(raw)
	fail := func(hint string) (map[string]any, error) {
		return nil, &Failure{Code: messages.ErrorProviderEndpointInvalid, Hint: hint, Err: fmt.Errorf("invalid endpoint")}
	}
	if trimmed == "" {
		return fail("endpoint_empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fail("endpoint_shape")
	}
	return map[string]any{"scheme": u.Scheme, "host": u.Host}, nil
}

func checkCredentialsShape(in Input) (map[string]any, error) {
	for _, name := range in.RequiredCredentials {
		if in.CredentialsSatisfied == nil || !in.CredentialsSatisfied(name) {
			return nil, &Failure{
				Code:   messages.ErrorProviderCredentialMissing,
				Hint:   "credential_missing",
				Detail: map[string]any{"field": name},
				Err:    fmt.Errorf("credential field %s is empty", name),
			}
		}
	}
	return map[string]any{"fields": len(in.RequiredCredentials)}, nil
}
