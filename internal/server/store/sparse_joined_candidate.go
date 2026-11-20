//go:build linux && dqlite

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// checkSparseJoinedFiles identifies a candidate shape, never a live member role.
// Additional native files retain the ordinary cold-database refusal boundary.
func checkSparseJoinedFiles(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("sparse joined identity requires a private plain directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if name != "info.yaml" && name != "cluster.yaml" && name != "dqlite-lock" {
			return fmt.Errorf("sparse joined candidate contains unexpected native entry %s", name)
		}
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 128<<10 || (name != "dqlite-lock" && info.Size() == 0) {
			return errors.New("sparse joined identity files must be private, regular and bounded")
		}
		found[name] = true
	}
	if !found["info.yaml"] || !found["cluster.yaml"] {
		return errors.New("sparse joined candidate requires its original identity and discovery cache")
	}
	return nil
}
