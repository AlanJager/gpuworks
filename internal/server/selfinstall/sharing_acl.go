package selfinstall

import (
	"fmt"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// preflightGroupSharing rejects ACLs before install writes files or changes
// supervisors. Rechecking after directory creation also catches inherited ACLs.
func (i *Installer) preflightGroupSharing(pf Preflight) error {
	if pf.Mode != ModeSystemd || pf.ServiceUser == "" || pf.ServiceUser == "root" {
		return nil
	}
	roots, err := i.permissionRoots()
	if err != nil {
		return err
	}
	return i.checkGroupSharingACLs(roots)
}

// chmod g+rwX also widens an extended ACL's mask. Check known secrets and
// their directory chains before any recursive grant, without scanning model
// directories or changing a custom directory's ACL.
func (i *Installer) checkGroupSharingACLs(roots []string) error {
	paths, err := sensitivePaths(i.layout)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := securefile.CheckFileACL(path); err != nil {
			return fmt.Errorf("refusing group permission changes: %w", err)
		}
	}
	directories := map[string]bool{}
	for _, root := range roots {
		directories[root] = true
	}
	for _, target := range []string{i.layout.DataDir, i.layout.BackupsDir} {
		target, err = resolveUninstallPath(target)
		if err != nil {
			return err
		}
		directories[target] = true
		for _, root := range roots {
			if !pathWithin(root, target) {
				continue
			}
			for parent := target; parent != root; parent = filepath.Dir(parent) {
				directories[parent] = true
			}
		}
	}
	for path := range directories {
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		// Shared-root installations include individual server files as roots.
		if !info.IsDir() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		opened, statErr := f.Stat()
		if statErr == nil && !os.SameFile(info, opened) {
			statErr = fmt.Errorf("directory changed while checking %s", path)
		}
		if statErr == nil {
			statErr = securefile.CheckDirectoryACL(f)
		}
		closeErr := f.Close()
		if statErr != nil {
			return fmt.Errorf("refusing group permission changes for %s: %w", path, statErr)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
