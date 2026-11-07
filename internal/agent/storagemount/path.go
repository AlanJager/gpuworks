package storagemount

import (
	"errors"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/storagerules"
)

// checkCanonicalStoragePath also handles directories not created yet by
// resolving their nearest existing ancestor. Run inside the mount guard: a
// path on an unresponsive filesystem must not stall the reconcile loop.
func checkCanonicalStoragePath(p string) error {
	if err := storagerules.CheckPathForm(p); err != nil {
		return err
	}
	ancestor := filepath.Clean(p)
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			if resolved != ancestor {
				return errors.New("storage paths require a canonical directory without symbolic links")
			}
			return nil
		}
		if !os.IsNotExist(err) || filepath.Dir(ancestor) == ancestor {
			return err
		}
		ancestor = filepath.Dir(ancestor)
	}
}
