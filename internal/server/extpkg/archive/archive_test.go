package archive_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg/archive"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
)

func built(t *testing.T) *extsign.Built {
	t.Helper()
	root := extsign.NewRoot(t, "1")
	pub := extsign.NewPublisher(t, "acme", nil, root)
	return extsign.Build(t, pub, extsign.Minimal("acme", "acme_backup", "1.0.0"))
}

func codeOf(err error) (string, map[string]any) {
	var pe *messages.ProductError
	if errors.As(err, &pe) {
		return pe.Code(), pe.Params()
	}
	return "", nil
}

// memSink keeps every member it receives.
type memSink map[string]*bytes.Buffer

func (m memSink) Create(path string) (io.WriteCloser, error) {
	b := &bytes.Buffer{}
	m[path] = b
	return nopWriteCloser{b}, nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestReadStreamsMembersAndHoldsSignatureMembers(t *testing.T) {
	b := built(t)
	b.Extra = []extsign.Entry{{Header: tar.Header{Name: "ui/", Typeflag: tar.TypeDir, Mode: 0o755}}}
	sink := memSink{}
	c, err := archive.Read(bytes.NewReader(b.Archive(t)), sink, archive.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Manifest, b.Manifest) || !bytes.Equal(c.Signature, b.ManifestSig) || !bytes.Equal(c.Certificates, b.PublisherCrt) {
		t.Fatal("signature members not returned byte for byte")
	}
	if len(c.Members) != len(b.Files) {
		t.Fatalf("members = %d, want %d", len(c.Members), len(b.Files))
	}
	for i, m := range c.Members {
		if i > 0 && c.Members[i-1].Path >= m.Path {
			t.Fatalf("members not sorted: %v", c.Members)
		}
		f := b.Files[m.Path]
		sum := sha256.Sum256(f.Body)
		if m.Size != int64(len(f.Body)) || m.SHA256 != hex.EncodeToString(sum[:]) || !bytes.Equal(sink[m.Path].Bytes(), f.Body) {
			t.Fatalf("member %s: got %+v", m.Path, m)
		}
	}
	if _, ok := sink[archive.ManifestMember]; ok {
		t.Fatal("signature member streamed to the sink")
	}
}

func TestReadRefusesUnsafeArchives(t *testing.T) {
	cases := []struct {
		name   string
		extra  extsign.Entry
		member string
		reason string
	}{
		{"symlink", extsign.Entry{Header: tar.Header{Name: "bin/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}}, "bin/link", archive.ReasonLink},
		{"hardlink", extsign.Entry{Header: tar.Header{Name: "bin/hard", Typeflag: tar.TypeLink, Linkname: "locales/en.json"}}, "bin/hard", archive.ReasonLink},
		{"absolute", extsign.Entry{Header: tar.Header{Name: "/etc/cron.d/x", Typeflag: tar.TypeReg, Mode: 0o644}, Body: []byte("x")}, "/etc/cron.d/x", archive.ReasonUnsafePath},
		{"dotdot", extsign.Entry{Header: tar.Header{Name: "ui/../../x", Typeflag: tar.TypeReg, Mode: 0o644}, Body: []byte("x")}, "ui/../../x", archive.ReasonUnsafePath},
		{"dot", extsign.Entry{Header: tar.Header{Name: "./x", Typeflag: tar.TypeReg, Mode: 0o644}, Body: []byte("x")}, "./x", archive.ReasonUnsafePath},
		{"backslash", extsign.Entry{Header: tar.Header{Name: `ui\x`, Typeflag: tar.TypeReg, Mode: 0o644}, Body: []byte("x")}, `ui\x`, archive.ReasonUnsafePath},
		{"fifo", extsign.Entry{Header: tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}}, "fifo", archive.ReasonSpecialFile},
		{"chardev", extsign.Entry{Header: tar.Header{Name: "dev", Typeflag: tar.TypeChar}}, "dev", archive.ReasonSpecialFile},
		{"duplicate", extsign.Entry{Header: tar.Header{Name: "locales/en.json", Typeflag: tar.TypeReg, Mode: 0o644}, Body: []byte("{}")}, "locales/en.json", archive.ReasonDuplicate},
		{"duplicate signature", extsign.Entry{Header: tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o644}, Body: []byte("{}")}, "manifest.json", archive.ReasonDuplicate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := built(t)
			b.Extra = []extsign.Entry{tc.extra}
			_, err := archive.Read(bytes.NewReader(b.Archive(t)), archive.Discard{}, archive.DefaultLimits())
			code, params := codeOf(err)
			if code != messages.ErrorPluginPackageMalformed || params["member"] != tc.member || params["reason"] != tc.reason {
				t.Fatalf("err = %v (%s %v), want %s at %s", err, code, params, tc.reason, tc.member)
			}
		})
	}
}

// Members must unpack to one tree on any filesystem: a name may not be both
// a file and a directory, in either order, and no two names may differ only
// in case.
func TestReadRefusesMembersThatClashAsATree(t *testing.T) {
	file := func(name string) extsign.Entry {
		return extsign.Entry{Header: tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}, Body: []byte("x")}
	}
	dir := func(name string) extsign.Entry {
		return extsign.Entry{Header: tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755}}
	}
	cases := []struct {
		name   string
		extra  []extsign.Entry
		member string
		reason string
	}{
		{"file then below it", []extsign.Entry{file("a"), file("a/b")}, "a/b", archive.ReasonUnsafePath},
		{"below then the file", []extsign.Entry{file("a/b/c"), file("a/b")}, "a/b", archive.ReasonUnsafePath},
		{"file then a directory entry", []extsign.Entry{file("a"), dir("a/")}, "a", archive.ReasonDuplicate},
		{"directory entry then a file", []extsign.Entry{dir("a/"), file("a")}, "a", archive.ReasonDuplicate},
		{"case twin", []extsign.Entry{file("Locales/en.json")}, "Locales/en.json", archive.ReasonDuplicate},
		{"file folding onto a directory", []extsign.Entry{file("LOCALES")}, "LOCALES", archive.ReasonUnsafePath},
		{"below a file of another case", []extsign.Entry{file("Readme"), file("readme/x")}, "readme/x", archive.ReasonUnsafePath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := built(t)
			b.Extra = tc.extra
			_, err := archive.Read(bytes.NewReader(b.Archive(t)), memSink{}, archive.DefaultLimits())
			code, params := codeOf(err)
			if code != messages.ErrorPluginPackageMalformed || params["member"] != tc.member || params["reason"] != tc.reason {
				t.Fatalf("err = %v (%s %v), want %s at %s", err, code, params, tc.reason, tc.member)
			}
		})
	}
	// A directory entry for a directory already implied is not a clash.
	b := built(t)
	b.Extra = []extsign.Entry{file("ui/x/y"), dir("ui/x/"), dir("ui/")}
	if _, err := archive.Read(bytes.NewReader(b.Archive(t)), memSink{}, archive.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}

func TestReadRequiresEverySignatureMember(t *testing.T) {
	for _, name := range []string{archive.ManifestMember, archive.SignatureMember, archive.CertificatesMember} {
		b := built(t)
		switch name {
		case archive.ManifestMember:
			b.Manifest = nil
		case archive.SignatureMember:
			b.ManifestSig = nil
		default:
			b.PublisherCrt = nil
		}
		_, err := archive.Read(bytes.NewReader(b.Archive(t)), archive.Discard{}, archive.DefaultLimits())
		code, params := codeOf(err)
		if code != messages.ErrorPluginPackageMalformed || params["member"] != name || params["reason"] != archive.ReasonMissing {
			t.Fatalf("without %s: err = %v %v", name, err, params)
		}
	}
}

func TestReadRefusesNonGzipAndTruncatedInput(t *testing.T) {
	for name, input := range map[string][]byte{
		"empty":     nil,
		"plain tar": []byte("manifest.json\x00\x00\x00"),
		"zip":       []byte("PK\x03\x04garbage"),
	} {
		_, err := archive.Read(bytes.NewReader(input), archive.Discard{}, archive.DefaultLimits())
		if code, params := codeOf(err); code != messages.ErrorPluginPackageMalformed || params["reason"] != archive.ReasonNotGzipTar {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	raw := built(t).Archive(t)
	for _, cut := range []int{20, len(raw) / 2, len(raw) - 9} {
		_, err := archive.Read(bytes.NewReader(raw[:cut]), archive.Discard{}, archive.DefaultLimits())
		if code, _ := codeOf(err); code != messages.ErrorPluginPackageMalformed {
			t.Fatalf("cut at %d: err = %v", cut, err)
		}
	}
	corrupt := append([]byte(nil), raw...)
	corrupt[len(corrupt)/2] ^= 0xff
	if _, err := archive.Read(bytes.NewReader(corrupt), archive.Discard{}, archive.DefaultLimits()); err == nil {
		t.Fatal("a corrupt archive was read")
	}
}

func TestReadEnforcesSizeAndMemberLimits(t *testing.T) {
	b := built(t)
	b.Files["ui/big.js"] = extsign.File{Body: bytes.Repeat([]byte("x"), 2048)}
	b.Files["node/images/app.tar"] = extsign.File{Body: bytes.Repeat([]byte("i"), 4096)}
	raw := b.Archive(t)

	lim := archive.DefaultLimits()
	lim.MaxBytes = 2048
	_, err := archive.Read(bytes.NewReader(raw), archive.Discard{}, lim)
	if code, params := codeOf(err); code != messages.ErrorPluginPackageTooLarge || params["max"] != int64(2048) {
		t.Fatalf("plain cap: err = %v %v", err, params)
	}

	lim = archive.DefaultLimits()
	lim.MaxImageBytes = 4095
	_, err = archive.Read(bytes.NewReader(raw), archive.Discard{}, lim)
	if code, params := codeOf(err); code != messages.ErrorPluginPackageTooLarge || params["member"] != "node/images/app.tar" {
		t.Fatalf("image cap: err = %v %v", err, params)
	}

	// Image bytes do not count against the plain cap.
	lim = archive.DefaultLimits()
	lim.MaxBytes = 2048 + 200
	if _, err := archive.Read(bytes.NewReader(raw), archive.Discard{}, lim); err != nil {
		t.Fatalf("images counted against the plain cap: %v", err)
	}

	lim = archive.DefaultLimits()
	lim.MaxMembers = 4
	_, err = archive.Read(bytes.NewReader(raw), archive.Discard{}, lim)
	if code, params := codeOf(err); code != messages.ErrorPluginPackageMalformed || params["reason"] != archive.ReasonTooManyMembers {
		t.Fatalf("member cap: err = %v %v", err, params)
	}

	big := built(t)
	big.Manifest = bytes.Repeat([]byte(" "), 4<<20+1)
	_, err = archive.Read(bytes.NewReader(big.Archive(t)), archive.Discard{}, archive.DefaultLimits())
	if code, params := codeOf(err); code != messages.ErrorPluginPackageTooLarge || params["member"] != archive.ManifestMember {
		t.Fatalf("oversized manifest: err = %v %v", err, params)
	}
}

func TestReadSkipsGlobalHeaders(t *testing.T) {
	b := built(t)
	b.Extra = []extsign.Entry{{Header: tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}}}}
	if _, err := archive.Read(bytes.NewReader(b.Archive(t)), archive.Discard{}, archive.DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}

type failingSink struct {
	createErr, writeErr, closeErr error
}

func (f failingSink) Create(string) (io.WriteCloser, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &failingWriter{f}, nil
}

type failingWriter struct{ f failingSink }

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.f.writeErr != nil {
		return 0, w.f.writeErr
	}
	return len(p), nil
}
func (w *failingWriter) Close() error { return w.f.closeErr }

func TestReadPropagatesSinkFailures(t *testing.T) {
	raw := built(t).Archive(t)
	disk := errors.New("disk full")
	for name, sink := range map[string]failingSink{
		"create": {createErr: disk},
		"write":  {writeErr: disk},
		"close":  {closeErr: disk},
	} {
		_, err := archive.Read(bytes.NewReader(raw), sink, archive.DefaultLimits())
		if !errors.Is(err, disk) {
			t.Fatalf("%s: err = %v, want the sink's error", name, err)
		}
		if code, _ := codeOf(err); code != "" {
			t.Fatalf("%s: a sink failure was reported as a package defect (%s)", name, code)
		}
	}
}

func TestSafePath(t *testing.T) {
	for p, want := range map[string]bool{
		"bin/linux_amd64/x": true, "a": true, "": false, "/a": false, "a//b": false, "a/./b": false,
		"a/../b": false, "a/": false, "a\x00b": false, strings.Repeat("a", 513): false,
	} {
		if got := archive.SafePath(p); got != want {
			t.Fatalf("SafePath(%q) = %v", p, got)
		}
	}
}
