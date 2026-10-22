package cloudflared

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

// maxBinaryBytes caps what an archive member may expand to. The real binary is
// ~40 MB; anything far beyond that is a decompression bomb or a corrupt
// length field, and either way it must not be written to disk.
const maxBinaryBytes = 200 << 20

// ExtractBinary pulls usr/bin/cloudflared out of a downloaded package and
// writes it to destPath (mode 0755). Both package formats are read in pure Go:
// a Debian package is an ar archive holding data.tar.gz, an RPM is a header
// followed by a gzip-compressed cpio payload. Nothing here shells out to dpkg,
// rpm or cpio, because the RPM family install we target does not guarantee
// cpio is present, and the operator may not be root.
func ExtractBinary(pkgPath string, format Format, destPath string) error {
	if err := os.MkdirAll(path.Dir(destPath), 0o2775); err != nil {
		return err
	}
	tmp := destPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	var extractErr error
	switch format {
	case FormatDeb:
		extractErr = extractDeb(pkgPath, f)
	case FormatRPM:
		extractErr = extractRPM(pkgPath, f)
	default:
		extractErr = fmt.Errorf("unknown package format %q", format)
	}
	if extractErr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return extractErr
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destPath)
}

func extractDeb(pkgPath string, w io.Writer) error {
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	member, names, err := arLookup(f, info.Size(), "data.tar.gz")
	if err != nil {
		return fmt.Errorf("%s: %w (members: %s)", pkgPath, err, strings.Join(names, ", "))
	}
	gz, err := gzip.NewReader(member)
	if err != nil {
		return fmt.Errorf("%s: data.tar.gz: %w", pkgPath, err)
	}
	defer gz.Close()
	if err := extractTarMember(gz, BinaryMember, w); err != nil {
		return fmt.Errorf("%s: %w", pkgPath, err)
	}
	return nil
}

func extractRPM(pkgPath string, w io.Writer) error {
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()
	offset, compressor, err := rpmPayloadOffset(f)
	if err != nil {
		return fmt.Errorf("%s: %w", pkgPath, err)
	}
	if compressor != "gzip" {
		// Cloudflare's current packages are gzip; xz or zstd would need another
		// reader, so say so plainly instead of failing as a corrupt archive.
		return fmt.Errorf("%s: unsupported payload compressor %q (this unpacker handles gzip)", pkgPath, compressor)
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: payload: %w", pkgPath, err)
	}
	defer gz.Close()
	if err := extractCPIOMember(gz, BinaryMember, w); err != nil {
		return fmt.Errorf("%s: %w", pkgPath, err)
	}
	return nil
}

// arLookup finds one member of an ar archive and also returns the member names
// it saw, so a miss can explain what the archive actually holds.
func arLookup(r io.ReaderAt, size int64, want string) (io.Reader, []string, error) {
	magic := make([]byte, 8)
	if _, err := r.ReadAt(magic, 0); err != nil {
		return nil, nil, err
	}
	if string(magic) != "!<arch>\n" {
		return nil, nil, fmt.Errorf("not an ar archive")
	}
	var names []string
	off := int64(8)
	for off+60 <= size {
		header := make([]byte, 60)
		if _, err := r.ReadAt(header, off); err != nil {
			break
		}
		name := strings.TrimSuffix(strings.TrimRight(string(header[0:16]), " "), "/")
		memberSize, err := strconv.ParseInt(strings.TrimSpace(string(header[48:58])), 10, 64)
		if err != nil || memberSize < 0 {
			return nil, names, fmt.Errorf("malformed ar member header at offset %d", off)
		}
		dataStart := off + 60
		names = append(names, name)
		if name == want {
			return io.NewSectionReader(r, dataStart, memberSize), names, nil
		}
		off = dataStart + memberSize
		if memberSize%2 == 1 {
			off++
		}
	}
	return nil, names, fmt.Errorf("no %s member", want)
}

// extractTarMember streams one regular file out of a tar stream.
func extractTarMember(r io.Reader, want string, w io.Writer) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if cleanArchivePath(hdr.Name) != want {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("%s is not a regular file (type %q); refusing to extract a link or device", want, hdr.Typeflag)
		}
		if hdr.Size > maxBinaryBytes {
			return fmt.Errorf("%s claims to be %d bytes; refusing to extract it", want, hdr.Size)
		}
		_, err = io.Copy(w, tr)
		return err
	}
	return fmt.Errorf("%s not found in data.tar.gz", want)
}

// cpioField indexes into the 13 hex fields of a "newc" header, which follow the
// 6-byte magic.
const (
	cpioFieldMode     = 1
	cpioFieldFileSize = 6
	cpioFieldNameSize = 11
)

// extractCPIOMember streams one file out of an RPM's cpio payload.
func extractCPIOMember(r io.Reader, want string, w io.Writer) error {
	br := bufio.NewReader(r)
	header := make([]byte, 110)
	for {
		if _, err := io.ReadFull(br, header); err != nil {
			return fmt.Errorf("cpio header: %w", err)
		}
		switch magic := string(header[0:6]); magic {
		case "070701", "070702":
		default:
			return fmt.Errorf("unsupported cpio magic %q", magic)
		}
		var fields [13]int64
		for i := range fields {
			v, err := strconv.ParseInt(string(header[6+i*8:6+(i+1)*8]), 16, 64)
			if err != nil {
				return fmt.Errorf("cpio field %d: %w", i, err)
			}
			fields[i] = v
		}
		nameSize := fields[cpioFieldNameSize]
		if nameSize <= 0 || nameSize > 4096 {
			return fmt.Errorf("cpio member name of %d bytes is out of range", nameSize)
		}
		nameBuf := make([]byte, nameSize)
		if _, err := io.ReadFull(br, nameBuf); err != nil {
			return err
		}
		name := strings.TrimRight(string(nameBuf), "\x00")
		if pad := pad4(110 + int(nameSize)); pad > 0 {
			if _, err := io.CopyN(io.Discard, br, int64(pad)); err != nil {
				return err
			}
		}
		if name == "TRAILER!!!" {
			return fmt.Errorf("%s not found in the RPM payload", want)
		}
		fileSize := fields[cpioFieldFileSize]
		if fileSize < 0 || fileSize > maxBinaryBytes {
			return fmt.Errorf("cpio member %s claims to be %d bytes; refusing to extract it", name, fileSize)
		}
		mode := fields[cpioFieldMode]
		if cleanArchivePath(name) == want {
			// 0o100000 is S_IFREG; a symlink or device here would make the
			// extracted "binary" point somewhere else entirely.
			if mode&0o170000 != 0o100000 {
				return fmt.Errorf("%s is not a regular file (mode %o)", want, mode)
			}
			if _, err := io.CopyN(w, br, fileSize); err != nil {
				return err
			}
			return nil
		}
		if _, err := io.CopyN(io.Discard, br, fileSize); err != nil {
			return err
		}
		if pad := pad4(int(fileSize)); pad > 0 {
			if _, err := io.CopyN(io.Discard, br, int64(pad)); err != nil {
				return err
			}
		}
	}
}

// cleanArchivePath normalises the "./usr/..." spelling both formats use.
func cleanArchivePath(name string) string {
	return path.Clean(strings.TrimPrefix(name, "./"))
}

// pad4 is the padding that brings n up to the next multiple of four, the
// alignment cpio newc members use for both names and data.
func pad4(n int) int { return (4 - n%4) % 4 }

// RPM header tags this package reads.
const (
	rpmTagPayloadFormat     = 1124
	rpmTagPayloadCompressor = 1125
)

// rpmPayloadOffset walks the lead, the signature header and the main header,
// and returns where the compressed payload starts plus the compressor the
// package declares.
func rpmPayloadOffset(r io.ReaderAt) (int64, string, error) {
	lead := make([]byte, 96)
	if _, err := r.ReadAt(lead, 0); err != nil {
		return 0, "", err
	}
	if !bytes.Equal(lead[0:4], []byte{0xed, 0xab, 0xee, 0xdb}) {
		return 0, "", fmt.Errorf("not an RPM package")
	}
	sigEnd, _, err := readRPMHeader(r, 96)
	if err != nil {
		return 0, "", fmt.Errorf("signature header: %w", err)
	}
	mainEnd, tags, err := readRPMHeader(r, sigEnd)
	if err != nil {
		return 0, "", fmt.Errorf("main header: %w", err)
	}
	if format := tags[rpmTagPayloadFormat]; format != "" && format != "cpio" {
		return 0, "", fmt.Errorf("unsupported payload format %q", format)
	}
	compressor := tags[rpmTagPayloadCompressor]
	if compressor == "" {
		// Packages built before the tag existed are gzip by definition.
		compressor = "gzip"
	}
	return payloadStart(r, mainEnd, compressor), compressor, nil
}

// payloadStart finds where the payload really begins. The main header's data
// section is described by its own size field, but whether the next section
// starts immediately or at the next eight-byte boundary depends on the builder,
// so the offset is confirmed against the compressor's magic instead of assumed.
func payloadStart(r io.ReaderAt, mainEnd int64, compressor string) int64 {
	magic := map[string][]byte{
		"gzip":  {0x1f, 0x8b},
		"bzip2": []byte("BZh"),
		"xz":    {0xfd, '7', 'z', 'X', 'Z', 0x00},
		"zstd":  {0x28, 0xb5, 0x2f, 0xfd},
	}[compressor]
	candidates := []int64{mainEnd, align8(mainEnd)}
	if magic == nil {
		return candidates[0]
	}
	buf := make([]byte, len(magic))
	for _, off := range candidates {
		if _, err := r.ReadAt(buf, off); err == nil && bytes.Equal(buf, magic) {
			return off
		}
	}
	return candidates[0]
}

// readRPMHeader parses one header structure (magic, index, data) and returns
// the offset where the next section starts plus its string-valued tags.
func readRPMHeader(r io.ReaderAt, off int64) (int64, map[int]string, error) {
	head := make([]byte, 16)
	if _, err := r.ReadAt(head, off); err != nil {
		return 0, nil, err
	}
	if head[0] != 0x8e || head[1] != 0xad || head[2] != 0xe8 {
		return 0, nil, fmt.Errorf("bad header magic %x at offset %d", head[0:3], off)
	}
	nindex := int64(binary.BigEndian.Uint32(head[8:12]))
	hsize := int64(binary.BigEndian.Uint32(head[12:16]))
	if nindex < 0 || nindex > 65536 || hsize < 0 || hsize > 64<<20 {
		return 0, nil, fmt.Errorf("implausible header at offset %d (index %d, size %d)", off, nindex, hsize)
	}
	dataStart := off + 16 + nindex*16
	tags := make(map[int]string)
	entries := make([]byte, nindex*16)
	if _, err := r.ReadAt(entries, off+16); err != nil {
		return 0, nil, err
	}
	buf := make([]byte, 1024)
	for i := int64(0); i < nindex; i++ {
		entry := entries[i*16 : (i+1)*16]
		tag := int(binary.BigEndian.Uint32(entry[0:4]))
		typ := binary.BigEndian.Uint32(entry[4:8])
		dataOff := int64(binary.BigEndian.Uint32(entry[8:12]))
		// 6 = STRING, 8 = STRING_ARRAY, 9 = I18NSTRING.
		if typ != 6 && typ != 8 && typ != 9 || dataOff < 0 || dataOff >= hsize {
			continue
		}
		n, err := r.ReadAt(buf, dataStart+dataOff)
		if n <= 0 && err != nil {
			continue
		}
		if value, _, ok := strings.Cut(string(buf[:n]), "\x00"); ok {
			tags[tag] = value
		}
	}
	return dataStart + hsize, tags, nil
}

func align8(n int64) int64 { return (n + 7) &^ 7 }

// ExtractDarwinTarball pulls cloudflared out of Cloudflare's macOS release
// asset, which is a tar.gz holding the binary at its root.
func ExtractDarwinTarball(tgzPath, destPath string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", tgzPath, err)
	}
	defer gz.Close()
	return extractTarMemberNamed(gz, "cloudflared", destPath)
}

// extractTarMemberNamed streams one regular file out of a tar into destPath,
// with the same refusals as the package path: no links, no devices, no absurd
// sizes.
func extractTarMemberNamed(r io.Reader, want, destPath string) error {
	tmp := destPath + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if err := extractTarMember(r, want, out); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destPath)
}
