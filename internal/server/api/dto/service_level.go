package dto

// ServiceLevel is PUT /deployments/{id}/service-level's body and response:
// resident, online, shared or offline.
type ServiceLevel struct {
	Level string `json:"level"`
}
