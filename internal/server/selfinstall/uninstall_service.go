package selfinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CheckUninstallServiceOverrides refuses ownership based on a base unit when a
// drop-in can change its effective command. Explicit searchDirs support isolated
// filesystem fixtures; callers normally use systemd's standard system paths.
func CheckUninstallServiceOverrides(unitPath string, searchDirs ...string) error {
	if len(searchDirs) == 0 {
		searchDirs = []string{
			"/etc/systemd/system.control", "/run/systemd/system.control",
			"/run/systemd/transient", "/run/systemd/generator.early",
			"/etc/systemd/system", "/etc/systemd/system.attached",
			"/run/systemd/system", "/run/systemd/system.attached",
			"/run/systemd/generator", "/usr/local/lib/systemd/system",
			"/usr/lib/systemd/system", "/lib/systemd/system", "/run/systemd/generator.late",
		}
	}
	searchDirs = append([]string{filepath.Dir(unitPath)}, searchDirs...)
	unit := filepath.Base(unitPath)
	suffix := filepath.Ext(unit)
	names := []string{unit + ".d", strings.TrimPrefix(suffix, ".") + ".d"}
	prefix := strings.TrimSuffix(unit, suffix)
	for n := strings.LastIndex(prefix, "-"); n >= 0; n = strings.LastIndex(prefix, "-") {
		names = append(names, prefix[:n+1]+suffix+".d")
		prefix = prefix[:n]
	}
	for _, root := range searchDirs {
		for _, name := range names {
			dir := filepath.Join(root, name)
			info, err := os.Lstat(dir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("cannot inspect systemd override directory %s; inspect it before uninstalling", dir)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".conf") && !strings.HasPrefix(entry.Name(), ".") {
					return fmt.Errorf("cannot establish service ownership with systemd overrides in %s; inspect them before uninstalling", dir)
				}
			}
		}
	}
	return nil
}
