package dto

// MCPStatus is whether the MCP service serves and how to reach it.
type MCPStatus struct {
	Enabled         bool     `json:"enabled"`
	Path            string   `json:"path"`
	ProtocolVersion string   `json:"protocol_version"`
	Tools           []string `json:"tools"`
}

// MCPSettings turns the MCP service on or off.
type MCPSettings struct {
	Enabled bool `json:"enabled"`
}
