package imagecatalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrRegistryLoginRejected is a registry refusing the credentials, as opposed
// to a registry that could not be reached or answered something unexpected.
var ErrRegistryLoginRejected = errors.New("the registry rejected the username or password")

// CheckRegistryLogin logs in to a registry the way a pull would: it asks
// /v2/ with the credentials, and when the registry answers with a bearer
// token challenge (Docker Hub, Harbor, GHCR, Quay) it asks the token service
// for a token with them. A 200 is a working login; a 401 or 403 from the
// token service or a basic-auth registry is ErrRegistryLoginRejected; anything
// else is an error that says what happened.
func CheckRegistryLogin(ctx context.Context, client *http.Client, host, username, secret string) error {
	base := "https://" + registryAPIHost(host) + "/v2/"
	resp, err := registryGet(ctx, client, base, username, secret)
	if err != nil {
		return err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	status := resp.StatusCode
	resp.Body.Close()
	switch {
	case status == http.StatusOK:
		return nil
	case status != http.StatusUnauthorized:
		return fmt.Errorf("%s answered %d", base, status)
	}
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") {
		// A basic-auth registry refused the credentials it was sent.
		return ErrRegistryLoginRejected
	}
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("%s asked for a token without saying where to get one", base)
	}
	tokenURL, err := url.Parse(realm)
	if err != nil || (tokenURL.Scheme != "https" && tokenURL.Scheme != "http") {
		return fmt.Errorf("%s named an unusable token service %q", base, realm)
	}
	query := tokenURL.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	tokenURL.RawQuery = query.Encode()
	resp, err = registryGet(ctx, client, tokenURL.String(), username, secret)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrRegistryLoginRejected
	}
	return fmt.Errorf("token service %s answered %d", tokenURL.Host, resp.StatusCode)
}

func registryGet(ctx context.Context, client *http.Client, target, username, secret string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(username, secret)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", req.URL.Host, err)
	}
	return resp, nil
}

// registryAPIHost is where a registry's API answers: Docker Hub serves it
// under another name than the one images are written with.
func registryAPIHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "docker.io" || host == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return host
}

// parseChallenge reads a WWW-Authenticate header: its scheme and its
// key="value" parameters.
func parseChallenge(header string) (string, map[string]string) {
	header = strings.TrimSpace(header)
	scheme, rest, _ := strings.Cut(header, " ")
	params := map[string]string{}
	for _, part := range splitChallenge(rest) {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		params[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return scheme, params
}

// splitChallenge splits challenge parameters on commas outside quotes.
func splitChallenge(s string) []string {
	var parts []string
	var current strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			current.WriteRune(r)
		case r == ',' && !quoted:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}
