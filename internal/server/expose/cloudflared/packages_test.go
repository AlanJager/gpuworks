package cloudflared

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

const testDebIndex = `Package: cloudflared
Version: 2026.8.2
Architecture: amd64
Filename: pool/main/c/cloudflared/cloudflared_2026.8.2_amd64.deb
Size: 18000000
SHA256: 1111111111111111111111111111111111111111111111111111111111111111

Package: cloudflared
Version: 2026.9.1
Architecture: amd64
Filename: pool/main/c/cloudflared/cloudflared_2026.9.1_amd64.deb
Size: 19160216
SHA256: 2222222222222222222222222222222222222222222222222222222222222222

Package: something-else
Version: 9999.0.0
Filename: pool/main/s/something/something_9999.0.0_amd64.deb
SHA256: 3333333333333333333333333333333333333333333333333333333333333333
`

func TestParseDebPackagesPicksNewest(t *testing.T) {
	entry, err := ParseDebPackages([]byte(testDebIndex), PackageName)
	if err != nil {
		t.Fatalf("ParseDebPackages: %v", err)
	}
	if entry.Version != "2026.9.1" {
		t.Errorf("Version = %q, want the newest stanza", entry.Version)
	}
	if entry.Filename != "pool/main/c/cloudflared/cloudflared_2026.9.1_amd64.deb" {
		t.Errorf("Filename = %q", entry.Filename)
	}
	if entry.Size != 19160216 {
		t.Errorf("Size = %d", entry.Size)
	}
	if entry.SHA256[:4] != "2222" {
		t.Errorf("SHA256 = %q, want the newest stanza's digest", entry.SHA256)
	}
}

func TestParseDebPackagesRejectsMissingDigest(t *testing.T) {
	index := "Package: cloudflared\nVersion: 1\nFilename: pool/main/c/cloudflared/x.deb\n\n"
	if _, err := ParseDebPackages([]byte(index), PackageName); err == nil {
		t.Fatal("an index entry without SHA256 must not be downloadable unverified")
	}
}

func TestParseDebPackagesMissingPackage(t *testing.T) {
	if _, err := ParseDebPackages([]byte("Package: other\nVersion: 1\nFilename: f\nSHA256: aa\n\n"), PackageName); err == nil {
		t.Fatal("want an error for a package that is not in the index")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2026.9.1", "2026.8.2", 1},
		{"2026.8.2", "2026.9.1", -1},
		{"2026.9.1", "2026.9.1", 0},
		{"2026.10", "2026.9.1", 1}, // numeric, not lexicographic
		{"2026.9", "2026.9.0", 0},  // missing parts are zero
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

const testRepoMD = `<?xml version="1.0" encoding="UTF-8"?>
<repomd xmlns="http://linux.duke.edu/metadata/repo">
  <data type="primary">
    <checksum type="sha256">abc</checksum>
    <location href="repodata/abc-primary.xml.zst"/>
    <size>966</size>
  </data>
  <data type="filelists">
    <location href="repodata/def-filelists.xml.zst"/>
  </data>
</repomd>`

func TestParseRepoMD(t *testing.T) {
	got, err := ParseRepoMD([]byte(testRepoMD))
	if err != nil {
		t.Fatalf("ParseRepoMD: %v", err)
	}
	if got != "repodata/abc-primary.xml.zst" {
		t.Fatalf("primary = %q", got)
	}
	if _, err := ParseRepoMD([]byte("<repomd></repomd>")); err == nil {
		t.Fatal("want an error when the metadata has no primary entry")
	}
}

const testPrimaryXML = `<metadata xmlns="http://linux.duke.edu/metadata/common">
<package type="rpm">
  <name>cloudflared</name>
  <arch>aarch64</arch>
  <version epoch="0" ver="2026.9.1" rel="1"/>
  <checksum type="sha256" pkgid="YES">aaaa</checksum>
  <location href="aarch64/cloudflared-linux-aarch64.rpm"/>
  <size package="19000000" installed="40000000"/>
</package>
<package type="rpm">
  <name>cloudflared</name>
  <arch>x86_64</arch>
  <version epoch="0" ver="2026.9.1" rel="1"/>
  <checksum type="sha256" pkgid="YES">bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb</checksum>
  <location href="x86_64/cloudflared-linux-x86_64.rpm"/>
  <size package="19100000" installed="40000000"/>
</package>
<package type="rpm">
  <name>other</name>
  <arch>x86_64</arch>
  <checksum type="sha256" pkgid="YES">cccc</checksum>
  <location href="x86_64/other.rpm"/>
</package>
</metadata>`

func TestParsePrimaryXMLSelectsArch(t *testing.T) {
	location, sha, size, err := ParsePrimaryXML([]byte(testPrimaryXML), PackageName, "x86_64")
	if err != nil {
		t.Fatalf("ParsePrimaryXML: %v", err)
	}
	if location != "x86_64/cloudflared-linux-x86_64.rpm" {
		t.Errorf("location = %q", location)
	}
	if len(sha) != 64 || !strings.HasPrefix(sha, "bbbb") {
		t.Errorf("sha256 = %q, want the x86_64 entry's digest", sha)
	}
	if size != 19100000 {
		t.Errorf("size = %d", size)
	}
	if _, _, _, err := ParsePrimaryXML([]byte(testPrimaryXML), PackageName, "riscv64"); err == nil {
		t.Error("an architecture that is not published must fail")
	}
}

// TestResolvePointsAtTheTestRepository drives both metadata paths against a
// local server, so the URLs, the digest and the size come from parsing rather
// than from a hand-written expectation.
func TestResolvePointsAtTheTestRepository(t *testing.T) {
	primary := compressZstd(t, []byte(testPrimaryXML))
	mux := http.NewServeMux()
	mux.HandleFunc("/dists/any/main/binary-amd64/Packages", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(testDebIndex))
	})
	mux.HandleFunc("/rpm/repodata/repomd.xml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(testRepoMD))
	})
	mux.HandleFunc("/rpm/repodata/abc-primary.xml.zst", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(primary)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	restore := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = restore }()

	amd64, err := ArchFor("amd64")
	if err != nil {
		t.Fatal(err)
	}
	deb, err := Resolve(context.Background(), srv.Client(), FormatDeb, amd64)
	if err != nil {
		t.Fatalf("Resolve(deb): %v", err)
	}
	if deb.URL != srv.URL+"/pool/main/c/cloudflared/cloudflared_2026.9.1_amd64.deb" {
		t.Errorf("deb URL = %q", deb.URL)
	}
	if !strings.HasPrefix(deb.SHA256, "2222") || deb.Size != 19160216 {
		t.Errorf("deb source = %+v", deb)
	}

	rpm, err := Resolve(context.Background(), srv.Client(), FormatRPM, amd64)
	if err != nil {
		t.Fatalf("Resolve(rpm): %v", err)
	}
	if rpm.URL != srv.URL+"/rpm/x86_64/cloudflared-linux-x86_64.rpm" {
		t.Errorf("rpm URL = %q", rpm.URL)
	}
	if !strings.HasPrefix(rpm.SHA256, "bbbb") || rpm.Size != 19100000 {
		t.Errorf("rpm source = %+v", rpm)
	}
}

func TestResolveRefusesTraversal(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/dists/any/main/binary-amd64/Packages", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("Package: cloudflared\nVersion: 1\nFilename: ../../etc/passwd\nSHA256: aa\n\n"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	restore := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = restore }()

	amd64, _ := ArchFor("amd64")
	if _, err := Resolve(context.Background(), srv.Client(), FormatDeb, amd64); err == nil {
		t.Fatal("a repository-supplied path escaping the pool must be refused")
	}
}

func TestResolveDebOnRPMOnlyArch(t *testing.T) {
	arch, err := ArchFor("386")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(context.Background(), http.DefaultClient, FormatDeb, arch); err == nil {
		t.Fatal("386 has no Debian package; Resolve must say so instead of building a URL")
	}
}

// compressZstd encodes repository metadata the way the real repodata does.
func compressZstd(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
