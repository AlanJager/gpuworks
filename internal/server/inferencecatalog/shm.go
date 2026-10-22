package inferencecatalog

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// MaxSpecShmBytes is the largest shm_bytes a spec may state: 16 TiB. A spec is
// reviewed text, not a measurement, so the bound only has to catch a value that
// is off by orders of magnitude (a GiB count written where bytes belong) before
// it becomes a --shm-size no host can honour.
const MaxSpecShmBytes int64 = 16 << 40

// validateShmBytes checks a spec's shm_bytes. Zero means the spec states none
// (an absent key and an explicit 0 read the same), so only a negative or an
// absurdly large value is refused, with the stable code
// CATALOG_SHM_BYTES_INVALID.
func validateShmBytes(modelID string, spec *InferenceSpec) error {
	if spec.ShmBytes >= 0 && spec.ShmBytes <= MaxSpecShmBytes {
		return nil
	}
	return messages.NewProductError(
		messages.ErrorCatalogShmBytesInvalid,
		fmt.Sprintf("model %s spec %s shm_bytes %d must be between 1 and %d (16 TiB)", modelID, spec.Name, spec.ShmBytes, MaxSpecShmBytes),
		map[string]any{"model": modelID, "spec": spec.Name, "shm_bytes": spec.ShmBytes, "max_bytes": MaxSpecShmBytes},
	)
}
