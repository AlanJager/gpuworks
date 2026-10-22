package cloudflared

import (
	"archive/tar"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const fakeBinary = "#!/bin/sh\necho 'cloudflared version 0.0.0 (fake)'\n"

// The fixtures below rebuild the container layouts the parsers must read. They
// are not a substitute for the real packages — real_package_test.go checks the
// header walk against pkg.cloudflare.com — but they are what makes the layout
// logic testable offline, including the shapes that must be refused.

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarBytes(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range sortedKeys(entries) {
		content := entries[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func cpioBytes(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	for _, name := range sortedKeys(entries) {
		writeCPIOEntry(&buf, name, entries[name])
	}
	writeCPIOEntry(&buf, "TRAILER!!!", nil)
	return buf.Bytes()
}

func writeCPIOEntry(w *bytes.Buffer, name string, content []byte) {
	nameBytes := append([]byte(name), 0)
	w.WriteString("070701")
	fields := []int{0, 0o100644, 0, 0, 1, 0, len(content), 0, 0, 0, 0, len(nameBytes), 0}
	for _, v := range fields {
		fmt.Fprintf(w, "%08X", v)
	}
	w.Write(nameBytes)
	if pad := (4 - (110+len(nameBytes))%4) % 4; pad > 0 {
		w.Write(make([]byte, pad))
	}
	w.Write(content)
	if pad := (4 - len(content)%4) % 4; pad > 0 {
		w.Write(make([]byte, pad))
	}
}

// writeArMember emits one ar member: a fixed 60-byte header, the data, and the
// padding byte that keeps members two-byte aligned.
func writeArMember(w *bytes.Buffer, name string, data []byte) {
	fmt.Fprintf(w, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(data))
	w.Write(data)
	if len(data)%2 == 1 {
		w.WriteByte('\n')
	}
}

// buildDeb assembles a Debian package whose data member carries entries.
func buildDeb(dataMember string, data []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("!<arch>\n")
	writeArMember(&buf, "debian-binary", []byte("2.0\n"))
	writeArMember(&buf, "control.tar.gz", []byte{0x1f, 0x8b, 0x08, 0x00})
	writeArMember(&buf, dataMember, data)
	return buf.Bytes()
}

// buildRPM assembles an RPM: lead, an empty signature header, the main header
// declaring format and compressor, then the compressed cpio payload.
func buildRPM(t *testing.T, compressor string, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(rpmLead())
	buf.Write(rpmHeader(nil))
	buf.Write(rpmHeader(map[int]string{
		rpmTagPayloadFormat:     "cpio",
		rpmTagPayloadCompressor: compressor,
	}))
	buf.Write(gzipBytes(t, cpioBytes(entries)))
	return buf.Bytes()
}

func rpmLead() []byte {
	lead := make([]byte, 96)
	copy(lead, []byte{0xed, 0xab, 0xee, 0xdb})
	lead[4] = 3 // major version
	return lead
}

// rpmHeader writes a header structure. The real signature header carries tags
// of its own; an empty one exercises the same walk, and the tag payload is
// what the parser reads anyway.
func rpmHeader(tags map[int]string) []byte {
	keys := sortedKeys(tags)
	var data, entries []byte
	for _, tag := range keys {
		entry := make([]byte, 16)
		binary.BigEndian.PutUint32(entry[0:], uint32(tag))
		binary.BigEndian.PutUint32(entry[4:], 6) // STRING
		binary.BigEndian.PutUint32(entry[8:], uint32(len(data)))
		binary.BigEndian.PutUint32(entry[12:], 1)
		entries = append(entries, entry...)
		data = append(data, []byte(tags[tag])...)
		data = append(data, 0)
	}
	out := []byte{0x8e, 0xad, 0xe8, 0x01, 0, 0, 0, 0}
	out = binary.BigEndian.AppendUint32(out, uint32(len(keys)))
	out = binary.BigEndian.AppendUint32(out, uint32(len(data)))
	out = append(out, entries...)
	out = append(out, data...)
	for len(out)%8 != 0 {
		out = append(out, 0)
	}
	return out
}

func sortedKeys[K cmp.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// extract runs ExtractBinary into a temp destination and returns its content.
func extract(t *testing.T, pkgPath string, format Format) (string, os.FileMode) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "cloudflared")
	if err := ExtractBinary(pkgPath, format, dest); err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	return string(content), info.Mode().Perm()
}

func TestExtractBinaryFromDeb(t *testing.T) {
	pkg := buildDeb("data.tar.gz", gzipBytes(t, tarBytes(t, map[string][]byte{
		"./usr/share/doc/cloudflared/README": []byte("docs"),
		"./usr/bin/cloudflared":              []byte(fakeBinary),
	})))
	content, mode := extract(t, writeTemp(t, "cloudflared.deb", pkg), FormatDeb)
	if content != fakeBinary {
		t.Errorf("extracted %q, want the binary", content)
	}
	if mode != 0o755 {
		t.Errorf("mode = %v, want 0755 (the binary has to be executable)", mode)
	}
}

func TestExtractBinaryFromRPM(t *testing.T) {
	pkg := buildRPM(t, "gzip", map[string][]byte{
		"./usr/share/doc/cloudflared/README": []byte("docs"),
		"./usr/bin/cloudflared":              []byte(fakeBinary),
	})
	content, mode := extract(t, writeTemp(t, "cloudflared.rpm", pkg), FormatRPM)
	if content != fakeBinary {
		t.Errorf("extracted %q, want the binary", content)
	}
	if mode != 0o755 {
		t.Errorf("mode = %v, want 0755", mode)
	}
}

// TestExtractRefusesUnknownCompressor is the guard for the day Cloudflare
// switches its RPM payload: the error has to name the compressor instead of
// failing later as a corrupt archive.
func TestExtractRefusesUnknownCompressor(t *testing.T) {
	pkg := buildRPM(t, "xz", map[string][]byte{"./usr/bin/cloudflared": []byte(fakeBinary)})
	dest := filepath.Join(t.TempDir(), "cloudflared")
	err := ExtractBinary(writeTemp(t, "cloudflared.rpm", pkg), FormatRPM, dest)
	if err == nil {
		t.Fatal("an xz payload must be refused")
	}
	if !strings.Contains(err.Error(), "xz") {
		t.Errorf("error %q should name the compressor", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Error("a failed extraction must not leave a destination behind")
	}
}

func TestExtractDebNamesTheMembersItFound(t *testing.T) {
	// A package whose data member is compressed differently: the error must say
	// what the archive actually holds.
	pkg := buildDeb("data.tar.xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00})
	err := ExtractBinary(writeTemp(t, "cloudflared.deb", pkg), FormatDeb, filepath.Join(t.TempDir(), "cloudflared"))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "data.tar.xz") {
		t.Errorf("error %q should list the members it saw", err)
	}
}

func TestExtractMissingMember(t *testing.T) {
	deb := buildDeb("data.tar.gz", gzipBytes(t, tarBytes(t, map[string][]byte{"./usr/bin/other": []byte("x")})))
	if err := ExtractBinary(writeTemp(t, "cloudflared.deb", deb), FormatDeb, filepath.Join(t.TempDir(), "cf")); err == nil {
		t.Fatal("a package without the binary must fail")
	}
	rpm := buildRPM(t, "gzip", map[string][]byte{"./usr/bin/other": []byte("x")})
	if err := ExtractBinary(writeTemp(t, "cloudflared.rpm", rpm), FormatRPM, filepath.Join(t.TempDir(), "cf")); err == nil {
		t.Fatal("an RPM without the binary must fail")
	}
}

func TestExtractTruncatedPackages(t *testing.T) {
	full := buildDeb("data.tar.gz", gzipBytes(t, tarBytes(t, map[string][]byte{"./usr/bin/cloudflared": []byte(fakeBinary)})))
	truncated := full[:len(full)/2]
	if err := ExtractBinary(writeTemp(t, "cloudflared.deb", truncated), FormatDeb, filepath.Join(t.TempDir(), "cf")); err == nil {
		t.Fatal("a truncated deb must fail rather than extract half a binary")
	}
	// Incompressible payload so the cut lands inside the payload rather than
	// inside the header.
	big := make([]byte, 8<<10)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	rpm := buildRPM(t, "gzip", map[string][]byte{"./usr/bin/cloudflared": big})
	if err := ExtractBinary(writeTemp(t, "cloudflared.rpm", rpm[:len(rpm)-2048]), FormatRPM, filepath.Join(t.TempDir(), "cf")); err == nil {
		t.Fatal("a truncated rpm payload must fail")
	}
}

func TestArLookupRejectsNonArchive(t *testing.T) {
	path := writeTemp(t, "not-an-archive.pkg", []byte("this is not an ar archive at all"))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	if _, _, err := arLookup(f, info.Size(), "data.tar.gz"); err == nil {
		t.Fatal("want an error for a file that is not an ar archive")
	}
}

func TestRPMHeaderRejectsNonRPM(t *testing.T) {
	path := writeTemp(t, "not-an-rpm.pkg", []byte(strings.Repeat("nope", 64)))
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, _, err := rpmPayloadOffset(f); err == nil {
		t.Fatal("want an error for a file without the RPM lead")
	}
}
