package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/distribution"
)

// packageArchive writes a release archive the way scripts/package_release.sh
// does: a gpuworks-<id>-<os>-<arch>/ directory with the binary and an
// INSTALL.txt that records the build time. The binary embeds the build time
// too, so two builds of one commit differ in bytes and size.
func packageArchive(t *testing.T, dir, name, builtAt string) {
	t.Helper()
	base := strings.TrimSuffix(name, ".tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{
		{base + "/gpuworks", "binary built " + builtAt + strings.Repeat("x", len(builtAt)%7)},
		{base + "/INSTALL.txt", "GPUWorks\n\nBuild commit: c0ffee\nBuild time:   " + builtAt + "\nDocumentation: x\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildDist(t *testing.T, builtAt string, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		packageArchive(t, dir, name, builtAt)
	}
	return dir
}

var abcArchives = []string{"gpuworks-abc123-linux-amd64.tar.gz", "gpuworks-abc123-darwin-arm64.tar.gz"}

// A second pipeline of a published commit republishes the first build's
// archives: the tree it assembles holds the same bytes at every path under
// releases/, so an --immutable upload changes nothing and a CDN copy of the
// first build stays right.
func TestAdoptRepublishesThePublishedRelease(t *testing.T) {
	first := realInput(t, buildDist(t, "2026-10-14T13:39:57+08:00", abcArchives...))
	first.BuiltAt = "2026-10-14T13:39:57+08:00"
	firstIndex, err := assemble(first)
	if err != nil {
		t.Fatal(err)
	}

	// The rebuild differs, which is what broke the 590da6b4 upgrade.
	rebuilt := buildDist(t, "2026-10-14T15:03:15+08:00", abcArchives...)
	a, _ := os.ReadFile(filepath.Join(first.DistDir, abcArchives[0]))
	b, _ := os.ReadFile(filepath.Join(rebuilt, abcArchives[0]))
	if bytes.Equal(a, b) {
		t.Fatal("fixture: a rebuild must produce other bytes")
	}

	published := filepath.Join(first.OutDir, "releases", "abc123")
	builtAt, err := adopt(published, "abc123")
	if err != nil || builtAt != "2026-10-14T13:39:57+08:00" {
		t.Fatalf("adopt = %q, %v", builtAt, err)
	}
	second := realInput(t, published)
	second.ReleaseID, second.BuiltAt = "abc123", builtAt
	secondIndex, err := assemble(second)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range firstIndex.Server.Files {
		got, err := secondIndex.Server.Asset(f.OS, f.Arch)
		if err != nil || got.File != f.File {
			t.Fatalf("%s/%s republished as %+v, %v; first published %+v", f.OS, f.Arch, got, err, f.File)
		}
	}
	if !bytes.Equal(readTree(t, first.OutDir, "releases/abc123/SHA256SUMS"), readTree(t, second.OutDir, "releases/abc123/SHA256SUMS")) {
		t.Fatal("SHA256SUMS changed between publishes of one release")
	}
	if secondIndex.Server.BuiltAt != firstIndex.Server.BuiltAt {
		t.Fatalf("built_at = %s, the archives were built %s", secondIndex.Server.BuiltAt, firstIndex.Server.BuiltAt)
	}
}

func TestAdoptRefusesAReleaseThatDoesNotCheckOut(t *testing.T) {
	cases := map[string]struct {
		mutate func(t *testing.T, dir string)
		id     string
		want   string
	}{
		"archive replaced after SHA256SUMS": {mutate: func(t *testing.T, dir string) {
			packageArchive(t, dir, abcArchives[0], "2026-10-14T15:03:15+08:00")
		}, want: "SHA256SUMS lists"},
		"archive missing": {mutate: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, abcArchives[1])); err != nil {
				t.Fatal(err)
			}
		}, want: "SHA256SUMS lists 2 archives"},
		"no SHA256SUMS": {mutate: func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "SHA256SUMS")); err != nil {
				t.Fatal(err)
			}
		}, want: "SHA256SUMS"},
		"garbled SHA256SUMS": {mutate: func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("nonsense\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "is not"},
		"another release": {id: "zz9", want: "not zz9"},
		"no build time": {mutate: func(t *testing.T, dir string) {
			rewriteWithSums(t, dir, func(name string) { writeFile(t, filepath.Join(dir, name), "not a tarball") })
		}, want: "gzip"},
		"mixed build times": {mutate: func(t *testing.T, dir string) {
			rewriteWithSums(t, dir, func(name string) {
				if name == abcArchives[1] {
					packageArchive(t, dir, name, "2026-10-14T15:03:15+08:00")
				}
			})
		}, want: "the others"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := realInput(t, buildDist(t, "2026-10-14T13:39:57+08:00", abcArchives...))
			if _, err := assemble(in); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(in.OutDir, "releases", "abc123")
			if tc.mutate != nil {
				tc.mutate(t, dir)
			}
			id := tc.id
			if id == "" {
				id = "abc123"
			}
			if got, err := adopt(dir, id); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("adopt = %q, %v; want error with %q", got, err, tc.want)
			}
		})
	}
}

// rewriteWithSums lets change rewrite archives and then refreshes SHA256SUMS,
// so the check that fails is the one after the digests.
func rewriteWithSums(t *testing.T, dir string, change func(name string)) {
	t.Helper()
	var sums strings.Builder
	for _, name := range []string{abcArchives[1], abcArchives[0]} {
		change(name)
		digest, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		sums.WriteString(digest + "  " + name + "\n")
	}
	writeFile(t, filepath.Join(dir, "SHA256SUMS"), sums.String())
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Before a channel switches, every source must already serve the files of
// the new index. A CDN that still holds an earlier build at the same path
// fails the check while the channel names the previous release.
func TestVerifyChecksACandidateIndexBeforeTheSwitch(t *testing.T) {
	stale := realInput(t, buildDist(t, "2026-10-14T13:39:57+08:00", abcArchives...))
	if _, err := assemble(stale); err != nil {
		t.Fatal(err)
	}
	next := realInput(t, buildDist(t, "2026-10-14T15:03:15+08:00", abcArchives...))
	next.BuiltAt = "2026-10-14T15:03:15+08:00"
	if _, err := assemble(next); err != nil {
		t.Fatal(err)
	}
	candidate := readTree(t, next.OutDir, "nightly/index.json")
	// Neither source has switched: both still serve the stale index, which
	// the candidate check must not read.
	writeFile(t, filepath.Join(next.OutDir, "nightly", "index.json"), string(readTree(t, stale.OutDir, "nightly/index.json")))
	good := serveTree(t, next.OutDir)
	cdn := serveTree(t, stale.OutDir)
	keys := distribution.Keys{"test": testPublic}

	var out bytes.Buffer
	err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL, cdn.URL}, Index: candidate, Keys: keys}, &out)
	if err == nil || !strings.Contains(out.String(), "ok   "+good.URL) || !strings.Contains(out.String(), "FAIL "+cdn.URL) ||
		!strings.Contains(out.String(), "the index lists") {
		t.Fatalf("candidate verify = %v\n%s", err, out.String())
	}
	out.Reset()
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL}, Index: candidate, ReleaseID: "abc123", Full: true, Keys: keys}, &out); err != nil {
		t.Fatalf("full candidate verify: %v\n%s", err, out.String())
	}
	// Without the candidate the same sources pass: they agree with the index
	// they serve, which is exactly what readers saw during the 590da6b4 window.
	out.Reset()
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{cdn.URL}, Keys: keys}, &out); err != nil {
		t.Fatalf("stale source against its own index: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := verify(context.Background(), verifyInput{Channel: "nightly", Sources: []string{good.URL}, Index: candidate}, &out); err == nil || !strings.Contains(out.String(), "candidate index") {
		t.Fatalf("an index the trusted keys do not sign = %v\n%s", err, out.String())
	}
	out.Reset()
	if err := verify(context.Background(), verifyInput{Channel: "release", Sources: []string{good.URL}, Index: candidate, Keys: keys}, &out); err == nil || !strings.Contains(out.String(), "not \"release\"") {
		t.Fatalf("a candidate for another channel = %v\n%s", err, out.String())
	}
}

func TestReleaseIDAndAdoptCommands(t *testing.T) {
	dist := buildDist(t, "2026-10-14T13:39:57+08:00", abcArchives...)
	var out, errOut bytes.Buffer
	if code := run([]string{"release-id", "-dist", dist}, nil, &out, &errOut); code != 0 || out.String() != "abc123\n" {
		t.Fatalf("release-id: %d %q %s", code, out.String(), errOut.String())
	}
	out.Reset()
	if code := run([]string{"release-id", "-dist", t.TempDir()}, nil, &out, &errOut); code != 1 {
		t.Fatalf("release-id of an empty dir: %d", code)
	}

	in := realInput(t, dist)
	if _, err := assemble(in); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	dir := filepath.Join(in.OutDir, "releases", "abc123")
	if code := run([]string{"adopt", "-dir", dir, "-release-id", "abc123"}, nil, &out, &errOut); code != 0 || out.String() != "2026-10-14T13:39:57+08:00\n" {
		t.Fatalf("adopt: %d %q %s", code, out.String(), errOut.String())
	}
	errOut.Reset()
	if code := run([]string{"adopt", "-dir", dir}, nil, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "required") {
		t.Fatalf("adopt without -release-id: %d %s", code, errOut.String())
	}
}
