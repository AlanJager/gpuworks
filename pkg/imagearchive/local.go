package imagearchive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ReadRepoTags reads top-level image metadata. With an uncompressed tar the
// tar reader seeks over layers, even when manifest.json is the last member.
func ReadRepoTags(ctx context.Context, file io.ReadSeeker) ([]string, error) {
	var magic [2]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var input io.Reader = file
	if isGzip(magic[:]) {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		input = archiveContextReader{ctx, gz}
	}
	reader := tar.NewReader(input)
	var ociTags []string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return ociTags, nil
		}
		if err != nil {
			return nil, err
		}
		name := normalizeEntry(header.Name)
		if header.Typeflag != tar.TypeReg || (name != "manifest.json" && name != "index.json") {
			continue
		}
		if header.Size > tailWindowLarge {
			return nil, fmt.Errorf("archive %s exceeds the %d byte metadata limit", name, tailWindowLarge)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, err
		}
		if name == "manifest.json" {
			if tags := taggedReferences(dockerRepoTags(data)); len(tags) > 0 {
				return tags, nil
			}
		} else {
			ociTags = taggedReferences(ociIndexTags(data))
		}
	}
}

// OCI ref-name annotations can be bare tags. Only a repository with an
// explicit tag can populate the custom image reference.
func taggedReferences(refs []string) []string {
	out := refs[:0]
	for _, ref := range refs {
		name := ref[strings.LastIndexByte(ref, '/')+1:]
		repo, tag, ok := strings.Cut(name, ":")
		if ok && repo != "" && tag != "" && !strings.ContainsAny(ref, "@ \t\r\n") {
			out = append(out, ref)
		}
	}
	return out
}

type archiveContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r archiveContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
