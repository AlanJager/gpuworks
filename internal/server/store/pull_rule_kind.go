package store

import (
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/imagecatalog/pullref"
	"gpuworks.ai/gpuworks/internal/server/sourceurl"
)

// Kinds of pull source rule. A rule's kind is read from its match, never
// stored: a match with an http(s) scheme names a site reached over HTTP
// (a model hub), anything else an image registry or reference prefix.
const (
	PullRuleImage = "image"
	PullRuleURL   = "url"
)

// Kind is PullRuleURL or PullRuleImage, as the rule's match says.
func (r PullSourceRuleSpec) Kind() string {
	if sourceurl.IsURL(r.Match) {
		return PullRuleURL
	}
	return PullRuleImage
}

// URLRule is a site rule as the fetch planner reads it.
func (r PullSourceRuleSpec) URLRule() sourceurl.Rule {
	return sourceurl.Rule{Match: r.Match, Endpoints: append([]string(nil), r.Endpoints...), AllowUpstream: r.AllowUpstream}
}

// normalizeRuleAddresses validates a rule's match and endpoints by the rule's
// kind and returns them spelled the way they are stored. Every endpoint must
// be of the match's kind: a registry cannot stand in for a site, nor the
// other way round. Blank endpoints are dropped and a repeated one refused.
func normalizeRuleAddresses(match string, endpoints []string) (string, []string, error) {
	match = strings.TrimSpace(match)
	url := sourceurl.IsURL(match)
	if url {
		if err := sourceurl.ValidateMatch(match); err != nil {
			return match, nil, pullSettingError("match", err)
		}
		match, _ = sourceurl.Normalize(match)
	} else if err := pullref.ValidateRuleMatch(match); err != nil {
		return match, nil, pullSettingError("match", err)
	}
	out := make([]string, 0, len(endpoints))
	seen := map[string]bool{}
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		var key string
		switch {
		case url && !sourceurl.IsURL(endpoint):
			return match, nil, pullSettingError("endpoints", fmt.Errorf("endpoint %q is not an http(s) address; a rule for %s offers other sites", endpoint, match))
		case url:
			normalized, err := sourceurl.Normalize(endpoint)
			if err != nil {
				return match, nil, pullSettingError("endpoints", err)
			}
			endpoint, key = normalized, normalized
		default:
			if err := pullref.ValidateRuleEndpoint(endpoint); err != nil {
				return match, nil, pullSettingError("endpoints", err)
			}
			key = strings.ToLower(strings.TrimSuffix(endpoint, "/"))
		}
		if seen[key] {
			return match, nil, pullSettingError("endpoints", fmt.Errorf("endpoint %q is listed twice", endpoint))
		}
		seen[key] = true
		out = append(out, endpoint)
	}
	return match, out, nil
}
