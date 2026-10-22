package dto

// SecuritySettings are the platform-wide security switches (store.SecurityPolicy).
type SecuritySettings struct {
	// PlaygroundPreviewNetwork lets the playground HTML preview make network
	// requests; off by default.
	PlaygroundPreviewNetwork bool `json:"playground_preview_network"`
}
