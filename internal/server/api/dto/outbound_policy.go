package dto

// ServerOutboundPolicy exposes configured permissions, without the implicit
// grants used internally when restrictions are disabled.
type ServerOutboundPolicy struct {
	RestrictPrivateHosts bool     `json:"restrict_private_hosts"`
	AllowPrivateHosts    []string `json:"allow_private_hosts"`
	Editable             bool     `json:"editable"`
	Revision             string   `json:"revision"`
}

// ServerOutboundPolicyUpdate requires all fields to prevent partial requests
// from accidentally relaxing the restriction mode.
type ServerOutboundPolicyUpdate struct {
	RestrictPrivateHosts *bool    `json:"restrict_private_hosts"`
	AllowPrivateHosts    []string `json:"allow_private_hosts"`
	Revision             string   `json:"revision"`
}
