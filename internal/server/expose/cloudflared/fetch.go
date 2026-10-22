package cloudflared

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// DownloadRequest describes one verified download.
type DownloadRequest struct {
	URL  string
	Dest string
	// SHA256 is the expected digest; an empty value skips verification, which
	// only happens for metadata-less sources.
	SHA256 string
	// Size is the expected byte count, 0 to skip the size check.
	Size int64
	// Attempts bounds the retry loop (default 4). Measured throughput to
	// pkg.cloudflare.com from a Chinese egress swings between 0.8 and 2.8 MB/s
	// and one run in five stalls mid-transfer, so retrying with a resumed
	// Range is the difference between "works" and "usually works".
	Attempts int
	// RetryDelay scales the pause between attempts (default two seconds per
	// attempt number); tests shorten it.
	RetryDelay time.Duration
	Progress   func(done, total int64)
}

// DefaultClient is the HTTP client for package and metadata downloads: no
// overall timeout (the link can be slow), but bounded connection setup and
// response headers, and a direct fallback when the environment's proxy
// configuration cannot reach pkg.cloudflare.com.
func DefaultClient() *http.Client {
	proxied := http.DefaultTransport.(*http.Transport).Clone()
	proxied.ResponseHeaderTimeout = 60 * time.Second
	proxied.TLSHandshakeTimeout = 30 * time.Second
	direct := proxied.Clone()
	direct.Proxy = nil
	return &http.Client{
		Transport: &fallbackTransport{proxied: proxied, direct: direct},
		Timeout:   30 * time.Minute,
	}
}

// fallbackTransport tries the proxy configuration the environment asked for and
// falls back to a direct connection when that fails at the transport level.
//
// Measured on a Chinese egress with a local proxy configured: the proxy
// answered pkg.cloudflare.com with an EOF after five seconds while the direct
// path served the same URL in under one. The proxy is right in general (it is
// how the operator reaches the internet at all), so it is tried first; only a
// transport-level failure — never an HTTP status — falls through, which keeps
// a proxied 404 or 403 meaningful.
type fallbackTransport struct {
	proxied http.RoundTripper
	direct  http.RoundTripper
}

func (t *fallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.proxied.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	// Only the bodyless GETs this package issues reach here, so the request can
	// be replayed as-is through the direct transport.
	resp, directErr := t.direct.RoundTrip(req.Clone(req.Context()))
	if directErr != nil {
		return nil, err // report the proxied failure: that is the configured path
	}
	return resp, nil
}

// Download fetches req.URL into req.Dest. A partial file is kept as
// Dest+".part" across attempts and resumed with a Range request; the digest is
// verified before the file is renamed into place, so a caller never sees an
// unverified package at Dest.
func Download(ctx context.Context, client *http.Client, req DownloadRequest) error {
	if client == nil {
		client = DefaultClient()
	}
	attempts := req.Attempts
	if attempts <= 0 {
		attempts = 4
	}
	part := req.Dest + ".part"
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := downloadOnce(ctx, client, req, part); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return err
			}
			wait := time.Duration(attempt) * 2 * time.Second
			if req.RetryDelay > 0 {
				wait = time.Duration(attempt) * req.RetryDelay
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if err := verify(part, req); err != nil {
			// A digest mismatch means the bytes on disk are wrong; resuming on
			// top of them would keep them wrong, so start over next attempt.
			_ = os.Remove(part)
			lastErr = err
			continue
		}
		if err := os.Rename(part, req.Dest); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("download %s failed after %d attempts: %w", req.URL, attempts, lastErr)
}

func downloadOnce(ctx context.Context, client *http.Client, req DownloadRequest, part string) error {
	have := int64(0)
	if info, err := os.Stat(part); err == nil {
		have = info.Size()
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		httpReq.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case have > 0 && resp.StatusCode == http.StatusPartialContent:
		if _, err := f.Seek(have, 0); err != nil {
			return err
		}
	case have > 0 && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// The file is already complete; verification decides whether it is good.
		return nil
	case resp.StatusCode == http.StatusOK:
		have = 0
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return err
		}
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	total := req.Size
	if resp.ContentLength > 0 {
		total = have + resp.ContentLength
	}
	writer := io.Writer(f)
	if req.Progress != nil {
		writer = &progressWriter{w: f, done: have, total: total, report: req.Progress}
	}
	if _, err := io.Copy(writer, resp.Body); err != nil {
		return err
	}
	if req.Progress != nil && total > 0 {
		req.Progress(total, total)
	}
	return f.Sync()
}

type progressWriter struct {
	w     io.Writer
	done  int64
	total int64
	// report is called at most every megabyte; the CLI throttles it further by
	// time so a fast link does not flood the terminal.
	lastReport int64
	report     func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	if p.report != nil && p.done-p.lastReport >= 1<<20 {
		p.lastReport = p.done
		p.report(p.done, p.total)
	}
	return n, err
}

func verify(path string, req DownloadRequest) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if req.Size > 0 && info.Size() != req.Size {
		return fmt.Errorf("size mismatch: got %d bytes, want %d", info.Size(), req.Size)
	}
	if req.SHA256 == "" {
		return nil
	}
	got, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, req.SHA256) {
		return fmt.Errorf("sha256 mismatch: got %s, want %s", got, req.SHA256)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
