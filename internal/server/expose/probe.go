package expose

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// healthBody is the shape /healthz answers with. The probe reads it so that a
// captive portal, a Cloudflare error page or a login screen — all of which
// answer 200 with HTML — cannot be mistaken for this server.
type healthBody struct {
	Status string `json:"status"`
}

// Prober checks whether an address really serves this server.
type Prober struct {
	// Client is the client for the public probe. It is direct on purpose: the
	// server's own proxy settings are for reaching the internet on its behalf,
	// and a probe that went through them would not test what a public client
	// sees.
	Client *http.Client
	// Timeout bounds one probe.
	Timeout time.Duration
}

// NewProber builds the production prober: TLS is verified, redirects are not
// followed across hosts, and the probe is direct.
func NewProber() *Prober {
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		ResponseHeaderTimeout: 15 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
	}
	return &Prober{
		Client: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			// Following a redirect to another host would prove someone answers,
			// not that this server does. The plan forbids it for exactly that
			// reason: an Access login page redirect is not readiness.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 0 && !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
					return fmt.Errorf("refusing to follow a redirect from %s to %s", via[0].URL.Host, req.URL.Host)
				}
				if len(via) >= 3 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
		Timeout: 30 * time.Second,
	}
}

// CheckPublic verifies the public address serves this server: HTTPS with a
// valid certificate, /healthz answering with the gpuworks health body.
func (p *Prober) CheckPublic(ctx context.Context, publicURL string) error {
	host, err := url.Parse(publicURL)
	if err != nil {
		return fmt.Errorf("invalid public address %q: %w", publicURL, err)
	}
	if host.Scheme != "https" {
		return fmt.Errorf("public address %q is not https", publicURL)
	}
	return p.check(ctx, strings.TrimRight(publicURL, "/")+"/healthz", fmt.Sprintf("the public address %s", publicURL))
}

// CheckLocal verifies the origin the connector points at answers the same way.
// A connector with a dead origin publishes nothing, and the difference between
// "the tunnel is down" and "the server is down" is exactly what an operator
// needs to know.
func (p *Prober) CheckLocal(ctx context.Context, origin string) error {
	return p.check(ctx, strings.TrimRight(origin, "/")+"/healthz", fmt.Sprintf("the local origin %s", origin))
}

func (p *Prober) check(ctx context.Context, target, what string) error {
	client := p.Client
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if client == nil {
		client = NewProber().Client
	}
	if client.Timeout > timeout {
		// Keep the caller's bound; a probe that hangs is not evidence of health.
		c := *client
		c.Timeout = timeout
		client = &c
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s is not reachable: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("%s answered but the body could not be read: %w", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered HTTP %d", what, resp.StatusCode)
	}
	var health healthBody
	if err := json.Unmarshal(body, &health); err != nil || health.Status != "ok" {
		// Something answered, but it is not this server — a login page, a
		// challenge, an error page. Never call that ready.
		return fmt.Errorf("%s answered 200 but not with this server's health response (an HTML page or a proxy in between?)", what)
	}
	return nil
}
