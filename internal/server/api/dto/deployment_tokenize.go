package dto

// TokenizeRequest is PUT /deployments/{id}/tokenize. Enabled is required:
// a body without it is refused rather than read as off.
type TokenizeRequest struct {
	Enabled *bool `json:"enabled"`
}
