package runveil

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeDriverHost lays out a host driver directory plus ICD and utility dirs.
func fakeDriverHost(t *testing.T) string {
	t.Helper()
	libs := t.TempDir()
	if err := os.WriteFile(filepath.Join(libs, "libcuda.so.550.54.15"), []byte("cuda"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("libcuda.so.550.54.15", filepath.Join(libs, "libcuda.so.1")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libs, "libnvidia-ml.so.550.54.15"), []byte("nvml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libs, "libunrelated.so"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	etc, bin := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(etc, "OpenCL/vendors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(etc, "OpenCL/vendors/nvidia.icd"), []byte("libnvidia-opencl.so.1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte("smi"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldEtc, oldBin := hostEtcDir, hostBinDir
	hostEtcDir, hostBinDir = etc, bin
	t.Cleanup(func() { hostEtcDir, hostBinDir = oldEtc, oldBin })
	return libs
}

func TestSetupNVIDIACopiesDriverFilesOnce(t *testing.T) {
	libs := fakeDriverHost(t)
	dir := container(t, nil)
	root := filepath.Join(dir, "ROOT")
	libDir := filepath.Join(root, multiarchDir())
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	note, err := SetupNVIDIA(dir, []string{libs})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "installed 5 nvidia driver files") {
		t.Fatalf("note %q", note)
	}
	if link, err := os.Readlink(filepath.Join(libDir, "libcuda.so.1")); err != nil || link != "libcuda.so.550.54.15" {
		t.Fatalf("symlink not recreated verbatim: %q %v", link, err)
	}
	info, err := os.Stat(filepath.Join(libDir, "libnvidia-ml.so.550.54.15"))
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("library copy mode %v err %v", info.Mode(), err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "usr/bin/nvidia-smi")); string(data) != "smi" {
		t.Fatalf("utility not copied: %q", data)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/OpenCL/vendors/nvidia.icd")); err != nil {
		t.Fatalf("ICD not copied: %v", err)
	}
	if _, err := os.Stat(filepath.Join(libDir, "libunrelated.so")); !os.IsNotExist(err) {
		t.Fatal("a library outside udocker's list was copied")
	}
	if _, err := os.Stat(filepath.Join(dir, nvidiaMarker)); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	// A second prepare of the same container is a quiet no-op.
	note, err = SetupNVIDIA(dir, []string{libs})
	if err != nil || !strings.Contains(note, "already installed") {
		t.Fatalf("second setup: %q %v", note, err)
	}
}

func TestSetupNVIDIASkipsLikeUdocker(t *testing.T) {
	libs := fakeDriverHost(t)

	dir := container(t, nil)
	if note, err := SetupNVIDIA(dir, nil); err != nil || !strings.Contains(note, "host nvidia libraries not found") {
		t.Fatalf("no host driver: %q %v", note, err)
	}
	if note, err := SetupNVIDIA(dir, []string{libs}); err != nil || !strings.Contains(note, "no guest directory") {
		t.Fatalf("no guest lib dir: %q %v", note, err)
	}

	// The guest already ships a driver file: udocker refuses the whole setup.
	libDir := filepath.Join(dir, "ROOT", multiarchDir())
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(libDir, "libcuda.so.1"), []byte("stub"), 0o644); err != nil {
		t.Fatal(err)
	}
	note, err := SetupNVIDIA(dir, []string{libs})
	if err != nil || !strings.Contains(note, "guest already provides") {
		t.Fatalf("existing file: %q %v", note, err)
	}
	if data, _ := os.ReadFile(filepath.Join(libDir, "libcuda.so.1")); string(data) != "stub" {
		t.Fatal("guest file was overwritten")
	}
	if _, err := os.Stat(filepath.Join(dir, nvidiaMarker)); !os.IsNotExist(err) {
		t.Fatal("marker written for a skipped setup")
	}
}

func TestSetupNVIDIAReportsCopyFailure(t *testing.T) {
	libs := fakeDriverHost(t)
	dir := container(t, nil)
	libDir := filepath.Join(dir, "ROOT", multiarchDir())
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(libDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(libDir, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if _, err := SetupNVIDIA(dir, []string{libs}); err == nil {
		t.Fatal("unwritable guest library directory reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, nvidiaMarker)); !os.IsNotExist(err) {
		t.Fatal("marker written after a failed copy")
	}
}

func TestHostNVIDIALibDirsSources(t *testing.T) {
	libs := fakeDriverHost(t)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "libnvidia-cfg.so.1"), []byte("cfg"), 0o644); err != nil {
		t.Fatal(err)
	}
	ldconfig := "3 libs found in cache `/etc/ld.so.cache'\n" +
		"\tlibcuda.so.1 (libc6,x86-64) => " + filepath.Join(libs, "libcuda.so.1") + "\n" +
		"\tlibz.so.1 (libc6,x86-64) => /lib/libz.so.1\n"
	got := HostNVIDIALibDirs(ldconfig, "/does/not/exist:"+other)
	realLibs, _ := filepath.EvalSymlinks(libs)
	realOther, _ := filepath.EvalSymlinks(other)
	for _, want := range []string{realLibs, realOther} {
		if !slices.Contains(got, want) {
			t.Errorf("dirs %q lack %q", got, want)
		}
	}
	if !slices.IsSorted(got) {
		t.Errorf("dirs not sorted: %q", got)
	}
}
