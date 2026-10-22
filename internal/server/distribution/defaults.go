package distribution

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultSources are the public sources every release is published to, in no
// particular order: the client asks all of them at once. Two are mainland
// object stores on their default domains, which need no ICP filing; the third
// serves everyone else (ADR 0040).
var DefaultSources = []string{
	"https://gpuworks-dl.oss-cn-shanghai.aliyuncs.com",
	"https://gpuworks-dl.obs.cn-east-3.myhuaweicloud.com",
	"https://dl.gpuworks.ai",
}

// trustedKeys are the index signing keys this build accepts, base64. A
// rotation ships the new key next to the old one for a release cycle before
// the old one is removed.
var trustedKeys = map[string]string{
	"gpuworks-dist-1": "0kHmfeeoXwhkPTTXSah/yKV3hF6HeQ2dvCZ7TIaaTLc=",
}

// TrustedKeys returns the keys built into this binary.
func TrustedKeys() Keys {
	keys := make(Keys, len(trustedKeys))
	for id, encoded := range trustedKeys {
		key, err := PublicKey(encoded)
		if err != nil {
			// The table is a compile-time constant; a bad entry is a build defect.
			panic(fmt.Sprintf("distribution: trusted key %s: %v", id, err))
		}
		keys[id] = key
	}
	return keys
}

// NormalizeSource validates a source base URL and strips trailing slashes.
func NormalizeSource(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("source %q: %w", raw, err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", fmt.Errorf("source %q: scheme must be http or https", raw)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("source %q has no host", raw)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("source %q must not carry a query or fragment", raw)
	}
	return strings.TrimRight(trimmed, "/"), nil
}

// NormalizeSources validates a list of sources, dropping duplicates.
func NormalizeSources(raw []string) ([]string, error) {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, source := range raw {
		normalized, err := NormalizeSource(source)
		if err != nil {
			return nil, err
		}
		if !seen[normalized] {
			seen[normalized] = true
			out = append(out, normalized)
		}
	}
	return out, nil
}
