package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// packageScratch copies scripts/package_release.sh into a scratch repository
// with the license files it packs, and puts a stand-in make first on PATH
// that records its arguments and writes the server binary it was asked for,
// so the packaging runs without building anything.
func packageScratch(t *testing.T) (repo, makeLog string, env []string) {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "package_release.sh"))
	if err != nil {
		t.Fatal(err)
	}
	repo = t.TempDir()
	stubs := t.TempDir()
	makeLog = filepath.Join(t.TempDir(), "make.log")
	files := map[string]string{
		filepath.Join(repo, "scripts", "package_release.sh"): string(script),
		filepath.Join(repo, "LICENSE"):                       "license",
		filepath.Join(repo, "NOTICE"):                        "notice",
		filepath.Join(repo, "LICENSES", "talloc.txt"):        "talloc",
		// The target is the last argument: server-<os>-<arch>-prebuilt-ui.
		filepath.Join(stubs, "make"): "#!/bin/sh\necho \"$@\" >> " + makeLog + "\n" +
			"for target; do :; done\nplatform=${target#server-}; platform=${platform%-prebuilt-ui}\n" +
			"mkdir -p bin && printf 'binary %s\\n' \"$platform\" > \"bin/gpuworks-$platform\"\n",
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env = append(os.Environ(), "PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GPUWORKS_RELEASE_CHANNEL=nightly", "GPUWORKS_RELEASE_ID=abc123", "GPUWORKS_BUILD_COMMIT=0123456789abcdef",
		"GPUWORKS_BUILT_AT=2026-11-16T00:00:00Z", "GPUWORKS_RELEASE_OSES=linux", "GPUWORKS_RELEASE_ARCHES=amd64",
		"GPUWORKS_DIST_DIR="+filepath.Join(repo, "dist"), "TMPDIR="+t.TempDir())
	return repo, makeLog, env
}

func tarMembers(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	members := map[string]string{}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(reader)
		members[header.Name] = string(body)
	}
}

// Another edition builds its own main package and names its packages after
// itself, while the binary inside stays gpuworks.
func TestPackageReleaseWithAnotherPrefixAndServerPackage(t *testing.T) {
	repo, makeLog, env := packageScratch(t)
	cmd := exec.Command("sh", filepath.Join(repo, "scripts", "package_release.sh"))
	cmd.Env = append(env, "GPUWORKS_SERVER_PKG=example.com/acme/cmd/gpuworks-acme", "GPUWORKS_PACKAGE_PREFIX=gpuworks-acme")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package: %v\n%s", err, out)
	}
	made, _ := os.ReadFile(makeLog)
	if !strings.Contains(string(made), "SERVER_PKG=example.com/acme/cmd/gpuworks-acme server-linux-amd64-prebuilt-ui") {
		t.Fatalf("make was asked = %q", made)
	}
	dist := filepath.Join(repo, "dist")
	members := tarMembers(t, filepath.Join(dist, "gpuworks-acme-abc123-linux-amd64.tar.gz"))
	if members["gpuworks-acme-abc123-linux-amd64/gpuworks"] != "binary linux-amd64\n" {
		t.Fatalf("members = %v", members)
	}
	if notes := members["gpuworks-acme-abc123-linux-amd64/INSTALL.txt"]; !strings.Contains(notes, "upgrade --archive gpuworks-acme-<release>-linux-amd64.tar.gz") {
		t.Fatalf("INSTALL.txt = %q", notes)
	}
	sums, _ := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	release, _ := os.ReadFile(filepath.Join(dist, "release.json"))
	if !strings.Contains(string(sums), "  gpuworks-acme-abc123-linux-amd64.tar.gz") || !strings.Contains(string(release), `"name": "gpuworks-acme-abc123-linux-amd64.tar.gz"`) {
		t.Fatalf("SHA256SUMS = %q\nrelease.json = %s", sums, release)
	}
	// gpuworks-dist reads the same names back under the same prefix: the
	// release id, and the directory as adopt checks a published release.
	var out, errOut bytes.Buffer
	if code := run([]string{"release-id", "-dist", dist, "-prefix", "gpuworks-acme"}, nil, &out, &errOut); code != 0 || out.String() != "abc123\n" {
		t.Fatalf("release-id = %d %q %s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"adopt", "-dir", dist, "-release-id", "abc123", "-prefix", "gpuworks-acme"}, nil, &out, &errOut); code != 0 || out.String() != "2026-11-16T00:00:00Z\n" {
		t.Fatalf("adopt = %d %q %s", code, out.String(), errOut.String())
	}
}

// Without the parameters the packages are the community ones, as before.
func TestPackageReleaseDefaultsToTheCommunityPackage(t *testing.T) {
	repo, makeLog, env := packageScratch(t)
	cmd := exec.Command("sh", filepath.Join(repo, "scripts", "package_release.sh"))
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package: %v\n%s", err, out)
	}
	made, _ := os.ReadFile(makeLog)
	if !strings.Contains(string(made), "SERVER_PKG=./cmd/gpuworks ") {
		t.Fatalf("make was asked = %q", made)
	}
	if _, id, err := findArchives(filepath.Join(repo, "dist"), "", ""); err != nil || id != "abc123" {
		t.Fatalf("findArchives = %q, %v", id, err)
	}
}

func TestPackageReleaseRefusesABadPrefix(t *testing.T) {
	for _, prefix := range []string{"../x", "Gpuworks", "-x", "gpu works"} {
		repo, makeLog, env := packageScratch(t)
		cmd := exec.Command("sh", filepath.Join(repo, "scripts", "package_release.sh"))
		cmd.Env = append(env, "GPUWORKS_PACKAGE_PREFIX="+prefix)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "invalid package prefix") {
			t.Fatalf("prefix %q: err=%v output=%q", prefix, err, out)
		}
		if _, err := os.Stat(makeLog); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("prefix %q: a refused prefix built something", prefix)
		}
	}
}
