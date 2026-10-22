package store

import "encoding/json"

// SecurityPolicy holds the platform-wide security switches an administrator
// sets. Every field defaults to the restrictive choice, so a missing row is
// the safe policy.
type SecurityPolicy struct {
	// PlaygroundPreviewNetwork lets the playground's HTML preview make network
	// requests: fetch/XHR/WebSocket and plain-http subresources. The preview
	// runs model output inside the user's browser, which can reach the
	// intranet, so it stays off unless an administrator turns it on. The
	// preview sandbox itself does not change either way (ADR 0037).
	PlaygroundPreviewNetwork bool `json:"playground_preview_network"`
}

// SecurityPolicy returns the current policy; a missing or corrupt value is the
// zero (restrictive) policy.
func (s *Store) SecurityPolicy() SecurityPolicy {
	var policy SecurityPolicy
	if raw := s.GetSetting(SettingSecurityPolicy); raw != "" {
		_ = json.Unmarshal([]byte(raw), &policy)
	}
	return policy
}

// SetSecurityPolicy replaces the policy.
func (s *Store) SetSecurityPolicy(policy SecurityPolicy) error {
	b, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	return s.SetSetting(SettingSecurityPolicy, string(b))
}
