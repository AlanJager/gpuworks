package api

import (
	"net/url"
	"strings"
)

// redactAcquisitionSecrets prevents credentials embedded in an explicit proxy
// URL from being returned by deployment read APIs or persisted snapshots.
func redactAcquisitionSecrets(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, value := range x {
			if strings.EqualFold(k, "proxy") {
				x[k] = redactProxy(value)
				continue
			}
			redactAcquisitionSecrets(value)
		}
	case []any:
		for _, value := range x {
			redactAcquisitionSecrets(value)
		}
	}
}

func redactProxy(v any) any {
	s, ok := v.(string)
	if !ok || s == "" {
		return v
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User("REDACTED")
	return u.String()
}
