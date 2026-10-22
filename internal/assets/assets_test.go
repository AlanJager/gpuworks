package assets

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func requireEmbedded(t *testing.T, read func(string) ([]byte, error), name string) {
	t.Helper()
	if name == "" {
		t.Skip("no asset for this architecture")
	}
	if _, err := read(name); err != nil {
		t.Skip("asset is not embedded in this binary")
	}
}

func TestExtractProotKeepsMatchingAsset(t *testing.T) {
	requireEmbedded(t, files.ReadFile, prootAssets[runtime.GOARCH])
	dest := filepath.Join(t.TempDir(), "runtime", "proot")
	if _, err := ExtractProot(ProotStaticNoFD, dest); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractProot(ProotStaticNoFD, dest); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("matching PRoot was rewritten instead of being kept in place")
	}
}

func TestExtractUdockerBundle(t *testing.T) {
	requireEmbedded(t, files.ReadFile, udockerBundleAssets[runtime.GOARCH])
	dest := filepath.Join(t.TempDir(), "offline", "udocker")
	got, err := ExtractUdockerBundle(dest)
	if err != nil {
		t.Fatal(err)
	}
	if got != dest {
		t.Fatalf("destination = %q, want %q", got, dest)
	}
	if st, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o755 {
		t.Fatalf("bundle root mode = %o, want 755", st.Mode().Perm())
	}
	for _, name := range []string{
		filepath.Join("udocker", "udocker"),
		filepath.Join("englib", "lib", "VERSION"),
		".gpuworks-bundle.sha256",
	} {
		if _, err := os.Stat(filepath.Join(dest, name)); err != nil {
			t.Fatalf("missing extracted %s: %v", name, err)
		}
	}
	version, err := os.ReadFile(filepath.Join(dest, "englib", "lib", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if string(version) != "1.2.11\n" {
		t.Fatalf("englib version = %q", version)
	}
	// Provisioning installs the patched PRoot; the bundle carries none.
	if stray, _ := filepath.Glob(filepath.Join(dest, "englib", "bin", "proot*")); len(stray) != 0 {
		t.Fatalf("bundle still ships stock PRoot: %v", stray)
	}

	// A second extraction must recognize the hash marker and keep an intact
	// bundle in place instead of replacing it.
	marker := filepath.Join(dest, "keep")
	if err := os.WriteFile(marker, []byte("same bundle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractUdockerBundle(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("matching bundle was unexpectedly replaced: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "._README.md")); !os.IsNotExist(err) {
		t.Fatalf("AppleDouble metadata was extracted: %v", err)
	}
}

func TestBundleRelativePathRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../escape", "udocker-offline/../../escape", "/absolute"} {
		if got, ok := bundleRelativePath(name); ok {
			t.Fatalf("bundleRelativePath(%q) = %q, true", name, got)
		}
	}
}

func TestExtractLibvgpuConcurrent(t *testing.T) {
	if !LibvgpuAvailable() {
		t.Skip("libvgpu.so is an x86-64 asset and is not embedded in this binary")
	}
	dest := filepath.Join(t.TempDir(), "dgpu", "libvgpu.so")
	const workers = 8
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ExtractLibvgpu(dest)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if st, err := os.Stat(dest); err != nil || st.Size() < 4096 {
		t.Fatalf("extracted libvgpu = %v, want a real asset", err)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Base(entry.Name())[:1] == "." && entry.Name() != filepath.Base(dest) {
			t.Fatalf("temporary libvgpu file left behind: %s", entry.Name())
		}
	}
}

func TestExtractIBWriteBW(t *testing.T) {
	requireEmbedded(t, perftestFiles.ReadFile, ibWriteBWAssets[runtime.GOARCH])
	dest := filepath.Join(t.TempDir(), "tools", "ib_write_bw")
	got, err := ExtractIBWriteBW(dest)
	if err != nil || got != dest {
		t.Fatalf("extract = %q, %v", got, err)
	}
	st, err := os.Stat(dest)
	if err != nil || st.Mode().Perm() != 0o755 || st.Size() < 100_000 {
		t.Fatalf("extracted file = %v, %v", st, err)
	}
	// An intact copy is kept, a damaged one replaced.
	if _, err := ExtractIBWriteBW(dest); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(dest)
	if !os.SameFile(st, after) {
		t.Fatal("intact ib_write_bw was rewritten")
	}
	if err := os.WriteFile(dest, []byte("broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractIBWriteBW(dest); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dest); st.Size() < 100_000 {
		t.Fatal("damaged ib_write_bw was kept")
	}
	if _, err := extractIBWriteBW("riscv64", dest); err == nil {
		t.Fatal("extracted for an architecture without a build")
	}
	if err := Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyMatchesThisBinary(t *testing.T) {
	if err := Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestEmbedKeepsOnlyThisArchitecture(t *testing.T) {
	name := prootAssets[runtime.GOARCH]
	if _, err := files.ReadFile(name); err != nil {
		t.Skip("this binary does not embed a proot")
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	if _, err := files.ReadFile(prootAssets[other]); err == nil {
		t.Fatalf("embedded proot for %s", other)
	}
	if _, err := perftestFiles.ReadFile(ibWriteBWAssets[other]); err == nil {
		t.Fatalf("embedded ib_write_bw for %s", other)
	}
	probe := "bin/runtime-assets/probe/partition-probe-linux-" + other + ".tar.gz"
	if _, err := files.ReadFile(probe); err == nil {
		t.Fatalf("embedded partition probe %s", probe)
	}
	if runtime.GOARCH == "arm64" && LibvgpuAvailable() {
		t.Fatal("arm64 agent embedded libvgpu.so")
	}
}
