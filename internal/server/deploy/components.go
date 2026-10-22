package deploy

// ComponentBinding is the immutable deployment-snapshot binding between one
// catalog component, its selected weight artifact, and its container mount.
// StoreModelID is an internal database projection used by the controller; it
// is never inferred again from the mutable catalog after deployment creation.
type ComponentBinding struct {
	Name            string `json:"name"`
	Role            string `json:"role"`
	OriginalModelID string `json:"model_id,omitempty"`
	Provider        string `json:"provider"`
	RepoID          string `json:"repo_id"`
	Revision        string `json:"revision,omitempty"`
	Subfolder       string `json:"subfolder,omitempty"`
	Quantization    string `json:"quantization,omitempty"`
	MountPath       string `json:"mount_path"`
	StoreModelID    uint   `json:"store_model_id"`
	WeightBytes     int64  `json:"weight_bytes,omitempty"`
}

func PrimaryComponent(components []ComponentBinding) (ComponentBinding, bool) {
	for _, component := range components {
		if component.Role == "primary" {
			return component, true
		}
	}
	return ComponentBinding{}, false
}
