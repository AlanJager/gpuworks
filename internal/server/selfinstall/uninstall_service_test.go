package selfinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUninstallSystemdOverridesAllSearchPathsAndPrefixes(t *testing.T) {
	for _, unit := range []string{"gpuworks.service", "gpuworks-expose.service"} {
		for _, rootName := range []string{"etc", "run", "vendor"} {
			dirs := []string{unit + ".d", "service.d"}
			if unit == "gpuworks-expose.service" {
				dirs = append(dirs, "gpuworks-.service.d")
			}
			for _, dropin := range dirs {
				t.Run(unit+"/"+rootName+"/"+dropin, func(t *testing.T) {
					root := t.TempDir()
					roots := []string{filepath.Join(root, "etc"), filepath.Join(root, "run"), filepath.Join(root, "vendor")}
					dir := filepath.Join(root, rootName, dropin)
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "override.conf"), []byte("[Service]\nExecStart=/other/service\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					if err := CheckUninstallServiceOverrides(filepath.Join(roots[0], unit), roots...); err == nil {
						t.Fatal("effective systemd override accepted")
					}
				})
			}
		}
	}
}

func TestUninstallSystemdOverridesAdjacentAndUnsafeDirectories(t *testing.T) {
	for _, kind := range []string{"adjacent", "symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "custom", "gpuworks.service")
			dir := path + ".d"
			if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "adjacent":
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "override.conf"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "missing"), dir); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(dir, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := CheckUninstallServiceOverrides(path, filepath.Join(root, "run")); err == nil {
				t.Fatal("unsafe override path accepted")
			}
		})
	}
}

func TestUninstallSystemdOverridesIgnoresUnrelatedAndNonConfigFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"other.service.d/override.conf", "gpuworks.service.d/README", "service.d/.ignored.conf"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckUninstallServiceOverrides(filepath.Join(root, "gpuworks.service"), root); err != nil {
		t.Fatal(err)
	}
}
