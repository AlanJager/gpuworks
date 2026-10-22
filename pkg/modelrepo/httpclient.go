package modelrepo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// MaxDirectoryRedirects bounds how many redirects one request against an HTTP
// directory may follow. A file server that bounces a request more than a few
// times is either misconfigured or deliberately walking the client somewhere
// else; either way the download is not going to be the model.
const MaxDirectoryRedirects = 5

// DirectoryClient returns the HTTP client every request against an HTTP
// directory goes through — the listing, the metadata files the server reads,
// and the weight files the agent downloads. It is the single place the
// transport rules for this source live, so the control plane and the node
// cannot disagree about which hosts a directory URL may reach:
//
//   - http and https only,
//   - no credentials embedded in any URL,
//   - at most MaxDirectoryRedirects redirects,
//   - every redirect must stay on the host the directory was registered with.
//
// The host rule is what keeps an operator-pasted URL from being walked off its
// mirror onto an unrelated server by a redirect the operator never saw. base
// may carry a proxy or timeouts (a node's network configuration); it is copied
// rather than mutated, and a nil base yields a plain client.
func DirectoryClient(directoryURL string, base *http.Client) (*http.Client, error) {
	normalized, err := NormalizeDirectoryURL(directoryURL)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return nil, fmt.Errorf("model directory URL %q is not a valid URL: %w", directoryURL, err)
	}
	host := parsed.Host
	client := &http.Client{}
	if base != nil {
		*client = *base
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxDirectoryRedirects {
			return fmt.Errorf("model directory request followed more than %d redirects: url=%s", MaxDirectoryRedirects, safeRequestURL(req.URL))
		}
		return checkDirectoryRedirect(req.URL, host)
	}
	return client, nil
}

// checkDirectoryRedirect applies the transport rules to one redirect target.
func checkDirectoryRedirect(target *url.URL, host string) error {
	if target == nil {
		return fmt.Errorf("model directory request redirected to no URL")
	}
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("model directory request redirected to unsupported scheme %q", target.Scheme)
	}
	if target.User != nil {
		return fmt.Errorf("model directory request redirected to a URL embedding credentials")
	}
	if !strings.EqualFold(target.Host, host) {
		return fmt.Errorf("model directory request redirected from host %s to %s", host, target.Host)
	}
	return nil
}

// HeadFile reports one file's exact size and content type. A directory index
// prints sizes for humans, so this is how an exact byte count is obtained when
// one is needed — and how a URL that turns out to name a file rather than a
// directory is recognized. A server that does not report a length yields 0,
// which means "unknown" rather than "empty".
func HeadFile(ctx context.Context, rawURL string, client *http.Client) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
	if err != nil {
		return 0, "", err
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("head %s: %w", safeRequestURL(req.URL), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("head %s: http %d", safeRequestURL(req.URL), resp.StatusCode)
	}
	size := resp.ContentLength
	if size < 0 {
		size = 0
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	return size, contentType, nil
}

// safeRequestURL strips anything credential-shaped before a URL reaches a log
// line or an error message.
func safeRequestURL(parsed *url.URL) string {
	if parsed == nil {
		return ""
	}
	redacted := *parsed
	redacted.User = nil
	redacted.RawQuery = ""
	redacted.Fragment = ""
	return redacted.String()
}
