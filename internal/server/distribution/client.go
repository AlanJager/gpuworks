package distribution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultIndexTimeout = 15 * time.Second
	defaultStallTimeout = 30 * time.Second
	// maxIndexBytes bounds an index read; a real one is a few KiB.
	maxIndexBytes = 1 << 20
)

// ErrNotAwaited marks a source whose index request was still running when
// another source had already won.
var ErrNotAwaited = errors.New("not needed: another source answered first")

// Client reads a channel from a set of sources. The zero value uses the
// default sources, the built-in keys and a client with bounded connect and
// header timeouts that honours the proxy environment.
type Client struct {
	Sources []string
	Keys    Keys
	HTTP    *http.Client
	// IndexTimeout bounds one source's index request.
	IndexTimeout time.Duration
	// StallTimeout abandons a file transfer that received nothing for this
	// long. A slow but moving transfer is never cut off.
	StallTimeout time.Duration
}

// Attempt is how one source answered an index request.
type Attempt struct {
	Source  string
	Latency time.Duration
	Err     error
}

// Fetched is a verified index and how it was obtained.
type Fetched struct {
	Index   Index
	Channel string
	// Source is the base URL that served the index; files are tried there first.
	Source string
	KeyID  string
	// Attempts has one entry per source, in the configured order.
	Attempts []Attempt
}

// SourcesError is returned when no source served a valid index.
type SourcesError struct {
	Channel  string
	Attempts []Attempt
}

// StatusError is a source answering an index request with a status other
// than 200.
type StatusError struct {
	URL    string
	Status int
	Text   string
}

func (e *StatusError) Error() string { return fmt.Sprintf("GET %s: %s", e.URL, e.Text) }

// ErrInvalidIndex marks an index a source served that did not verify or
// parse: a forged or damaged file, another channel's index, or a page put in
// its place (a captive portal).
var ErrInvalidIndex = errors.New("invalid index")

// Why no source served an index, from the most to the least specific.
const (
	// ReasonUnpublished: a source answered that the channel has no index,
	// none served a bad one. The channel is reachable and empty.
	ReasonUnpublished = "unpublished"
	// ReasonInvalid: a source served an index that did not verify.
	ReasonInvalid = "invalid"
	// ReasonUnreachable: no source answered with an index or with "not
	// found" — network, proxy, DNS, timeouts or server errors.
	ReasonUnreachable = "unreachable"
)

// Reason says why no source served an index. One "not found" is enough for
// unpublished: every source carries the same tree, so the others failing is
// only how they are reached.
func (e *SourcesError) Reason() string {
	invalid := false
	for _, attempt := range e.Attempts {
		var status *StatusError
		if errors.As(attempt.Err, &status) && status.Status == http.StatusNotFound {
			return ReasonUnpublished
		}
		if errors.Is(attempt.Err, ErrInvalidIndex) {
			invalid = true
		}
	}
	if invalid {
		return ReasonInvalid
	}
	return ReasonUnreachable
}

func (e *SourcesError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, attempt := range e.Attempts {
		parts = append(parts, fmt.Sprintf("%s: %v", attempt.Source, attempt.Err))
	}
	return fmt.Sprintf("no distribution source served a valid %s index (%s)", e.Channel, strings.Join(parts, "; "))
}

// FetchIndex asks every source at once and returns as soon as one index
// verifies; a dead or slow source therefore adds no wait.
func (c *Client) FetchIndex(ctx context.Context, channel string) (*Fetched, error) {
	return c.race(ctx, channel, false, nil)
}

// FetchIndexReporting is FetchIndex for an update check: the installation
// describes itself with report, query parameters the publisher reads from the
// sources' access logs (usagereport.FleetReport). Only the built-in sources
// get it — a client pointed at sources of its own (--source, an internal
// release site) asks for the index and says nothing else.
func (c *Client) FetchIndexReporting(ctx context.Context, channel string, report url.Values) (*Fetched, error) {
	if len(c.Sources) > 0 {
		report = nil
	}
	return c.race(ctx, channel, false, report)
}

// ProbeIndex asks every source and waits for all of them, each bounded by the
// index timeout, so a status view can show how every source answered. The
// index is still the first that verified.
func (c *Client) ProbeIndex(ctx context.Context, channel string) (*Fetched, error) {
	return c.race(ctx, channel, true, nil)
}

type indexResult struct {
	attempt Attempt
	index   Index
	keyID   string
}

func (c *Client) race(ctx context.Context, channel string, waitAll bool, report url.Values) (*Fetched, error) {
	if !ValidChannel(channel) {
		return nil, fmt.Errorf("channel %q is invalid", channel)
	}
	sources, err := c.sources()
	if err != nil {
		return nil, err
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Buffered for every source, so the requests a winner leaves behind finish
	// and exit without anyone reading them.
	results := make(chan indexResult, len(sources))
	for _, source := range sources {
		go func(source string) {
			start := time.Now()
			index, keyID, err := c.indexFrom(raceCtx, source, channel, report)
			results <- indexResult{attempt: Attempt{Source: source, Latency: time.Since(start), Err: err}, index: index, keyID: keyID}
		}(source)
	}
	answered := make(map[string]Attempt, len(sources))
	var winner *indexResult
	for range sources {
		var r indexResult
		select {
		case r = <-results:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		answered[r.attempt.Source] = r.attempt
		if r.attempt.Err == nil && winner == nil {
			won := r
			winner = &won
			if !waitAll {
				cancel()
				break
			}
		}
	}
	attempts := make([]Attempt, 0, len(sources))
	for _, source := range sources {
		attempt, ok := answered[source]
		if !ok {
			attempt = Attempt{Source: source, Err: ErrNotAwaited}
		}
		attempts = append(attempts, attempt)
	}
	if winner == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, &SourcesError{Channel: channel, Attempts: attempts}
	}
	return &Fetched{Index: winner.index, Channel: channel, Source: winner.attempt.Source, KeyID: winner.keyID, Attempts: attempts}, nil
}

// indexFrom reads, verifies and parses one source's index.
func (c *Client) indexFrom(ctx context.Context, source, channel string, report url.Values) (Index, string, error) {
	payload, keyID, err := c.signedFrom(ctx, source, channel+"/index.json", report.Encode())
	if err != nil {
		return Index{}, "", err
	}
	index, err := ParseIndex(payload, channel)
	if err != nil {
		return Index{}, "", fmt.Errorf("%s/%s/index.json: %w: %w", source, channel, ErrInvalidIndex, err)
	}
	return index, keyID, nil
}

// signedFrom reads one signed envelope from a source and returns its verified
// payload. query, when set, rides on the request but is not part of what is
// named in errors.
func (c *Client) signedFrom(ctx context.Context, source, path, query string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.indexTimeout())
	defer cancel()
	endpoint := source + "/" + path
	requested := endpoint
	if query != "" {
		requested += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requested, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", &StatusError{URL: endpoint, Status: resp.StatusCode, Text: resp.Status}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", endpoint, err)
	}
	if len(raw) > maxIndexBytes {
		return nil, "", fmt.Errorf("%s is larger than %d bytes: %w", endpoint, maxIndexBytes, ErrInvalidIndex)
	}
	payload, keyID, err := Open(raw, c.keys())
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w: %w", endpoint, ErrInvalidIndex, err)
	}
	return payload, keyID, nil
}

func (c *Client) sources() ([]string, error) {
	raw := c.Sources
	if len(raw) == 0 {
		raw = DefaultSources
	}
	sources, err := NormalizeSources(raw)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, errors.New("no distribution source is configured")
	}
	return sources, nil
}

func (c *Client) keys() Keys {
	if c.Keys != nil {
		return c.Keys
	}
	return TrustedKeys()
}

func (c *Client) indexTimeout() time.Duration {
	if c.IndexTimeout > 0 {
		return c.IndexTimeout
	}
	return defaultIndexTimeout
}

func (c *Client) stallTimeout() time.Duration {
	if c.StallTimeout > 0 {
		return c.StallTimeout
	}
	return defaultStallTimeout
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTPClient
}

// defaultHTTPClient bounds connecting and waiting for headers but not the body:
// a transfer is cut off only by the stall timeout, never for being slow.
var defaultHTTPClient = &http.Client{Transport: func() http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = 15 * time.Second
	return transport
}()}

// NewHTTPClient keeps distribution connect, header and streaming timeout behavior.
func NewHTTPClient(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	transport := defaultHTTPClient.Transport.(*http.Transport).Clone()
	transport.Proxy = proxy
	return &http.Client{Transport: transport}
}
