package pullref

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoAllowedSource is a node with nothing to pull an image from: every rule
// that matches the image forbids its own registry and offers no endpoint.
var ErrNoAllowedSource = errors.New("no pull source is allowed")

// SourceRule is one pull source rule as the resolver reads it: images whose
// reference Match names are tried at each of Endpoints first, in order, and at
// their own address afterwards unless AllowUpstream is false. Which nodes a
// rule applies to is decided before the resolver sees it, so every rule handed
// to PlanCandidates already applies to the node being planned for.
//
// Match is either a registry host ("docker.io", "ghcr.io",
// "registry.example:5000"), which covers every image on that registry, or a
// reference prefix ("gpuworks/", "quay.io/team/app"), which covers the images
// under it. An endpoint takes the place of whatever Match matched, so a host
// rule keeps the repository path ("docker.io" → "harbor/dockerhub-proxy" turns
// "nginx:1" into "harbor/dockerhub-proxy/library/nginx:1") and a prefix rule
// keeps what follows the prefix.
type SourceRule struct {
	Match         string
	Endpoints     []string
	AllowUpstream bool
}

// PlannedCandidate is one reference a node tries for an image. Rank marks the
// image's own copies: the node may reorder a run of ranked candidates by how
// well it reaches their registries, while unranked candidates (a rule's
// endpoints) keep the order the rules give them.
type PlannedCandidate struct {
	Ref    string
	Source string
	Rank   bool
}

// Copies lists the references that name the same image as canonical, in the
// order they are preferred: the further sources an inference template
// declared, then the registry sources the image catalog records, then
// canonical itself. Archive URLs in the catalog's list are files to download,
// not references, and are left out; so is anything that is not a usable
// reference. Duplicates keep their first spelling (RefKey).
func Copies(canonical string, templateSources, registrySources []string) []string {
	canonical = strings.TrimSpace(canonical)
	catalog := make([]string, 0, len(registrySources)+1)
	for _, ref := range registrySources {
		ref = strings.TrimSpace(ref)
		if ref == "" || IsArchiveURL(ref) || !usableRef(ref) {
			continue
		}
		catalog = append(catalog, ref)
	}
	if canonical != "" {
		catalog = append(catalog, canonical)
	}
	return DeclaredFirst(templateSources, catalog)
}

// PlanCandidates is the one ordering of where a node pulls an image from. The
// endpoints of the rules come first, rule by rule in the order given and, within
// a rule, copy by copy; the image's own copies follow, marked Rank. A copy that
// a matching rule forbids reaching directly is left out. The result can be
// empty only when every copy is forbidden and no rule offers an endpoint; that
// is an error, because the node would have nothing to try.
func PlanCandidates(canonical string, copies []string, rules []SourceRule) ([]PlannedCandidate, error) {
	canonical = strings.TrimSpace(canonical)
	if len(copies) == 0 && canonical != "" {
		copies = []string{canonical}
	}
	out := make([]PlannedCandidate, 0, len(copies)*2)
	seen := make(map[string]struct{}, len(copies)*2)
	add := func(ref string, rank bool) {
		key := RefKey(ref)
		if key == "" {
			return
		}
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, PlannedCandidate{Ref: ref, Source: Source(ref, canonical), Rank: rank})
	}
	upstream := make([]bool, len(copies))
	for i := range upstream {
		upstream[i] = true
	}
	for _, rule := range rules {
		for i, ref := range copies {
			suffix, ok := ruleSuffix(rule.Match, ref)
			if !ok {
				continue
			}
			if !rule.AllowUpstream {
				upstream[i] = false
			}
			for _, endpoint := range rule.Endpoints {
				endpoint = strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
				if endpoint == "" {
					continue
				}
				add(endpoint+suffix, false)
			}
		}
	}
	for i, ref := range copies {
		if upstream[i] {
			add(strings.TrimSpace(ref), true)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w for %s: every rule that matches it forbids pulling from its own registry and offers no endpoint", ErrNoAllowedSource, canonical)
	}
	return out, nil
}

// CandidateRefs is the plain ordered projection of PlanCandidates, for a
// consumer that cannot reorder ranked candidates (an agent that predates
// ranking) and simply tries them in turn.
func CandidateRefs(candidates []PlannedCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Ref)
	}
	return out
}

// RuleMatchesHost reports whether a rule's Match names a whole registry rather
// than a reference prefix: a single segment that looks like a host.
func RuleMatchesHost(match string) bool {
	match = strings.TrimSuffix(strings.TrimSpace(match), "/")
	return match != "" && !strings.Contains(match, "/") && HostLike(match)
}

// ValidateRuleMatch checks that a rule's Match is a registry host or a
// reference prefix. A URL, whitespace, a digest or a tag is not.
func ValidateRuleMatch(match string) error {
	match = strings.TrimSpace(match)
	if strings.Trim(match, "/") == "" {
		return fmt.Errorf("match is empty")
	}
	return validateRefPart(match)
}

// ValidateRuleEndpoint checks one endpoint of a rule: a registry host or a
// path on one, which the matched part of a reference is replaced with.
func ValidateRuleEndpoint(endpoint string) error {
	endpoint = strings.TrimSpace(endpoint)
	if strings.Trim(endpoint, "/") == "" {
		return fmt.Errorf("endpoint is empty")
	}
	if err := validateRefPart(endpoint); err != nil {
		return err
	}
	head, _, _ := strings.Cut(strings.TrimSuffix(endpoint, "/"), "/")
	if !HostLike(head) {
		return fmt.Errorf("endpoint %q must start with a registry host", endpoint)
	}
	return nil
}

func validateRefPart(value string) error {
	switch {
	case strings.Contains(value, "://"):
		return fmt.Errorf("%q is a URL; write a registry host or path without a scheme", value)
	case strings.ContainsAny(value, " \t\r\n"):
		return fmt.Errorf("%q contains whitespace", value)
	case strings.Contains(value, "@"):
		return fmt.Errorf("%q contains a digest", value)
	}
	last := value
	if i := strings.LastIndex(value, "/"); i >= 0 {
		last = value[i+1:]
	}
	if strings.Contains(last, ":") && strings.Contains(value, "/") {
		return fmt.Errorf("%q contains a tag", value)
	}
	return nil
}

// ruleSuffix reports whether match covers ref and returns what of ref follows
// the matched part, separator included, so that endpoint+suffix is the
// rewritten reference.
func ruleSuffix(match, ref string) (string, bool) {
	normalized := NormalizeRef(ref)
	if normalized == "" {
		return "", false
	}
	if RuleMatchesHost(match) {
		host := canonicalHost(strings.TrimSuffix(strings.TrimSpace(match), "/"))
		refHost, path, ok := strings.Cut(normalized, "/")
		if !ok || canonicalHost(refHost) != host || path == "" {
			return "", false
		}
		return "/" + path, true
	}
	prefix := NormalizePrefix(match)
	if prefix == "" {
		return "", false
	}
	if normalized == prefix {
		return "", true
	}
	if strings.HasPrefix(normalized, prefix+"/") || strings.HasPrefix(normalized, prefix+":") || strings.HasPrefix(normalized, prefix+"@") {
		return strings.TrimPrefix(normalized, prefix), true
	}
	return "", false
}

// canonicalHost folds the spellings Docker Hub is reached under into one, so a
// rule for "docker.io" covers an image written against index.docker.io.
func canonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return "docker.io"
	}
	return host
}

func usableRef(ref string) bool {
	if strings.ContainsAny(ref, " \t\r\n") || strings.Contains(ref, "://") {
		return false
	}
	return NormalizeRef(ref) != ""
}
