// Package sourceurl is the grammar of pull source rules for sites reached
// over HTTP (model hubs such as Hugging Face and ModelScope): what a rule may
// match, what it may offer instead, and the one ordering of where a node
// fetches from. It performs no I/O and knows neither models nor nodes; the
// registry counterpart for images is imagecatalog/pullref.
package sourceurl

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ErrNoAllowedSource is a fetch with nothing to try: every rule that matches
// the site forbids reaching it directly and offers no endpoint.
var ErrNoAllowedSource = errors.New("no fetch source is allowed")

// Rule is one pull source rule for an HTTP site as the planner reads it:
// addresses under Match are tried at each of Endpoints first, in order, and at
// their own address afterwards unless AllowUpstream is false. Which nodes a
// rule applies to is decided before the planner sees it.
//
// Match is a site's root address ("https://huggingface.co"). An endpoint
// takes its place and may carry a path: "https://huggingface.co" →
// "https://mirror.example/hf" makes the node reach the hub's
// "/api/models/x" at "https://mirror.example/hf/api/models/x".
type Rule struct {
	Match         string
	Endpoints     []string
	AllowUpstream bool
}

// Candidate is one base address a node tries. Upstream marks the site's own
// address, as opposed to an endpoint a rule offered.
type Candidate struct {
	Base     string
	Upstream bool
}

// IsURL reports whether a rule's match names an HTTP site rather than an
// image registry: it starts with http:// or https://. This is the only thing
// that tells the two kinds of rule apart.
func IsURL(match string) bool {
	lower := strings.ToLower(strings.TrimSpace(match))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// Normalize spells an address the way rules compare it: lower-case scheme
// and host, no default port, no trailing slash; the path keeps its case. It
// refuses anything that is not a plain http(s) prefix: credentials belong in
// the source credentials, and a query or fragment cannot be a prefix.
func Normalize(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	shown := redact(raw)
	if raw == "" {
		return "", fmt.Errorf("address is empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", fmt.Errorf("%q contains whitespace", shown)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL", shown)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%q must start with http:// or https://", shown)
	}
	if u.User != nil {
		return "", fmt.Errorf("%q contains a user name or password; add a credential for the host instead", shown)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("%q contains a query or fragment", shown)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%q has no host", shown)
	}
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	hostport := host
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		hostport = "[" + host + "]"
	}
	return scheme + "://" + hostport + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// redact hides the user information of an address so an error never repeats
// a password someone typed into a rule.
func redact(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	authority, path, _ := strings.Cut(rest, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = "xxxxx@" + authority[at+1:]
	}
	if path != "" || strings.Contains(rest, "/") {
		return scheme + "://" + authority + "/" + path
	}
	return scheme + "://" + authority
}

// ValidateMatch checks a URL rule's match: a site's root address. A rule is
// compared with the address a site is reached at (a hub's own root), never
// with the paths requested below it, so a match with a path would never
// apply and is refused instead of silently doing nothing.
func ValidateMatch(match string) error {
	normalized, err := Normalize(match)
	if err != nil {
		return err
	}
	rest := normalized[strings.Index(normalized, "://")+3:]
	if strings.Contains(rest, "/") {
		return fmt.Errorf("%q has a path; a rule matches a whole site such as https://huggingface.co", strings.TrimSpace(match))
	}
	return nil
}

// ValidateEndpoint checks one endpoint of a URL rule.
func ValidateEndpoint(endpoint string) error {
	_, err := Normalize(endpoint)
	return err
}

// Host is the host (and non-default port) of an address, or "" when it does
// not parse; credentials are keyed by it.
func Host(address string) string {
	normalized, err := Normalize(address)
	if err != nil {
		return ""
	}
	rest := normalized[strings.Index(normalized, "://")+3:]
	host, _, _ := strings.Cut(rest, "/")
	return host
}

// suffix reports whether match covers address and returns what of address
// follows the matched prefix. A prefix ends on a path segment boundary:
// "https://huggingface.co" covers "https://huggingface.co/api" but neither
// "https://huggingface.co.example" nor a subdomain.
func suffix(match, address string) (string, bool) {
	prefix, err := Normalize(match)
	if err != nil {
		return "", false
	}
	normalized, err := Normalize(address)
	if err != nil {
		return "", false
	}
	if normalized == prefix {
		return "", true
	}
	if strings.HasPrefix(normalized, prefix+"/") {
		return normalized[len(prefix):], true
	}
	return "", false
}

// Plan is the one ordering of where a node fetches from a site whose own
// address is base: the endpoints of the rules first, rule by rule in the order
// given, then base itself unless a matching rule forbids it. Duplicates keep
// their first place. The result is empty only when every matching rule
// forbids the site and none offers an endpoint; that is ErrNoAllowedSource.
func Plan(base string, rules []Rule) ([]Candidate, error) {
	own, err := Normalize(base)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(rules)+1)
	seen := map[string]bool{}
	add := func(address string, upstream bool) {
		if seen[address] {
			return
		}
		seen[address] = true
		out = append(out, Candidate{Base: address, Upstream: upstream})
	}
	upstream := true
	for _, rule := range rules {
		rest, ok := suffix(rule.Match, own)
		if !ok {
			continue
		}
		if !rule.AllowUpstream {
			upstream = false
		}
		for _, endpoint := range rule.Endpoints {
			normalized, err := Normalize(endpoint)
			if err != nil {
				continue
			}
			add(normalized+rest, false)
		}
	}
	if upstream {
		add(own, true)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w for %s: every rule that matches it forbids reaching it directly and offers no endpoint", ErrNoAllowedSource, own)
	}
	return out, nil
}

// Bases is the plain ordered projection of Plan.
func Bases(candidates []Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Base)
	}
	return out
}
