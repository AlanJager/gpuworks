package gpu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedLibraryPathSkipsStaleCacheCandidates(t *testing.T) {
	root := t.TempDir()
	const name = "libgpuworks-test-candidate.so"
	missing := filepath.Join(root, "missing", name)
	dangling := filepath.Join(root, "dangling.so")
	if err := os.Symlink(missing, dangling); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(root, name)
	if err := os.WriteFile(valid, []byte("library fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, first := range []string{missing, dangling} {
		t.Run(filepath.Base(first), func(t *testing.T) {
			fakeCLI(t, "ldconfig", fmt.Sprintf("printf '%%s\\n' '%s (libc6,x86-64) => %s' '%s (libc6,x86-64) => %s'\n", name, first, name, valid))
			got, err := sharedLibraryPath(t.Context(), name)
			if err != nil || got != valid {
				t.Fatalf("library path=%q err=%v, want existing candidate %q", got, err, valid)
			}
		})
	}
}

func TestSharedLibraryPathRejectsMissingAndNonRegularCandidates(t *testing.T) {
	root := t.TempDir()
	const name = "libgpuworks-test-unavailable.so"
	for _, tc := range []struct {
		name, path string
		missing    bool
	}{
		{"missing", filepath.Join(root, name), true},
		{"directory", root, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeCLI(t, "ldconfig", fmt.Sprintf("printf '%%s\\n' '%s (libc6,x86-64) => %s'\n", name, tc.path))
			got, err := sharedLibraryPath(t.Context(), name)
			if got != "" || err == nil || errors.Is(err, os.ErrNotExist) != tc.missing {
				t.Fatalf("unusable candidate path=%q err=%v", got, err)
			}
		})
	}
}

func TestHygonProbeLibrariesDoNotMountStaleTinfo(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "removed-tinfo5.so")
	var listing strings.Builder
	fmt.Fprintf(&listing, "libtinfo.so.5 (libc6,x86-64) => %s\n", stale)
	for _, name := range []string{"libz.so.1", "libtinfo.so.6", "libelf.so.1"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("library fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&listing, "%s (libc6,x86-64) => %s\n", name, path)
	}
	fakeCLI(t, "ldconfig", "cat <<'LIBS'\n"+listing.String()+"LIBS\n")
	libs, err := (&HygonProber{}).PartitionProbeLibraries(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, lib := range libs {
		info, err := os.Stat(lib.Source)
		if err != nil || !info.Mode().IsRegular() || lib.Source == stale {
			t.Fatalf("invalid probe library source %+v: %v", lib, err)
		}
	}
}

func TestSharedLibraryPathPreservesPermissionAndCancellation(t *testing.T) {
	root := t.TempDir()
	const name = "libgpuworks-test-permission.so"
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(private, name)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(private, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(private, 0o700) })
	fakeCLI(t, "ldconfig", fmt.Sprintf("printf '%%s\\n' '%s (libc6,x86-64) => %s'\n", name, path))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sharedLibraryPath(ctx, name); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrPermission) {
		t.Skip("current user can bypass directory permissions")
	}
	if got, err := sharedLibraryPath(context.Background(), name); got != "" || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("permission failure path=%q err=%v", got, err)
	}
}
