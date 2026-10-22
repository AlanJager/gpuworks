package loadgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var credentialKey = regexp.MustCompile(`(?i)(secret|password|passwd|token|api[_-]?key|access[_-]?key|private[_-]?key|credential|authorization|(?:^|[_-])(?:key|auth)(?:$|[_-]))`)
var diagnosticURLStart = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://`)

// RedactErrorText treats free-form endpoint and historical diagnostics as
// untrusted. A URL's end cannot be inferred from spaces, quotes or escapes in
// arbitrary text, so its entire remainder is explicitly omitted. Generated
// response text is separate and is never passed through this helper.
func RedactErrorText(text string, knownSecrets ...string) string {
	if location := diagnosticURLStart.FindStringIndex(text); location != nil {
		text = text[:location[0]] + "[REDACTED URL] [diagnostic suffix omitted]"
	}
	return redactKnownSecrets(text, knownSecrets)
}

// RedactURL accepts one complete URL, such as an endpoint snapshot or a typed
// url.Error.URL. It must not parse URLs out of free-form diagnostics.
func RedactURL(raw string) string {
	if len(diagnosticURLStart.FindAllStringIndex(raw, 2)) > 1 {
		return "[REDACTED URL]"
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "[REDACTED URL]"
	}
	parsed.User = nil
	// Fragments are never sent in an HTTP request. Historical snapshots may
	// still carry credential-bearing fragments, so they are not endpoint facts
	// suitable for a portable report.
	parsed.Fragment, parsed.RawFragment = "", ""
	query := parsed.Query()
	for key := range query {
		if credentialKey.MatchString(key) {
			query.Set(key, "[REDACTED]")
			continue
		}
		for i, value := range query[key] {
			if diagnosticURLStart.MatchString(value) {
				query[key][i] = "[REDACTED URL]"
			}
		}
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func redactKnownSecrets(text string, knownSecrets []string) string {
	var secrets []string
	for _, secret := range knownSecrets {
		if secret == "" {
			continue
		}
		encoded, _ := json.Marshal(secret)
		secrets = append(secrets, secret, string(encoded[1:len(encoded)-1]), url.QueryEscape(secret), url.PathEscape(secret))
	}
	// Replace longer secrets first so overlapping values cannot leave a suffix.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, secret := range secrets {
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
	}
	return text
}

// Keep the original chain for cancellation and transport classification while
// exposing only the sanitized diagnostic string to consumers.
type redactedError struct {
	cause error
	text  string
}

func (e redactedError) Error() string { return e.text }
func (e redactedError) Unwrap() error { return e.cause }

func (c Client) redactError(err error) error {
	if err == nil {
		return nil
	}
	secrets := []string{c.APIKey}
	if endpoint, parseErr := url.Parse(c.BaseURL); parseErr == nil {
		if endpoint.User != nil {
			secrets = append(secrets, endpoint.User.Username())
			if password, ok := endpoint.User.Password(); ok {
				secrets = append(secrets, password)
			}
		}
		for key, values := range endpoint.Query() {
			if credentialKey.MatchString(key) {
				secrets = append(secrets, values...)
			}
		}
	}
	diagnostic := err.Error()
	var transport *url.Error
	if errors.As(err, &transport) {
		// The boundary comes from net/http's typed error, never guessed text.
		// A scheme-free safe endpoint retains useful location/cause facts
		// through later generic JSONL/report redaction passes.
		endpoint := RedactURL(transport.URL)
		if _, address, ok := strings.Cut(endpoint, "://"); ok {
			endpoint = "//" + address
		}
		diagnostic = fmt.Sprintf("%s %q: %s", transport.Op, endpoint, RedactErrorText(transport.Err.Error()))
	}
	text := RedactErrorText(diagnostic, secrets...)
	if text == err.Error() {
		return err
	}
	return redactedError{cause: err, text: text}
}
