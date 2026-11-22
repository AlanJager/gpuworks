package imagearchive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
)

// ReadRepoTags returns the archive's tagged references.
func ReadRepoTags(ctx context.Context, file io.ReadSeeker) ([]string, error) {
	contents, err := ReadContents(ctx, file)
	return contents.RepoTags, err
}

// ReadContents reads top-level image metadata. With an uncompressed tar the
// tar reader seeks over layers, even when manifest.json is the last member.
func ReadContents(ctx context.Context, file io.ReadSeeker) (Contents, error) {
	var magic [2]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return Contents{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Contents{}, err
	}
	var input io.Reader = file
	if isGzip(magic[:]) {
		gz, err := gzip.NewReader(file)
		if err != nil {
			return Contents{}, err
		}
		defer gz.Close()
		input = archiveContextReader{ctx, gz}
	}
	return readTar(ctx, input, archiveMetadata{})
}

func readTar(ctx context.Context, input io.Reader, metadata archiveMetadata) (Contents, error) {
	reader := tar.NewReader(input)
	// Only retain a bounded amount of optional config/manifest data from a
	// local archive. Continue looking for the canonical tag list afterwards.
	var configBytes int64
	for {
		if err := ctx.Err(); err != nil {
			return Contents{}, err
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return metadata.contents(), nil
		}
		if err != nil {
			return metadata.contents(), err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := normalizeEntry(header.Name)
		switch name {
		case "manifest.json", "index.json":
			if header.Size > tailWindowLarge {
				return Contents{}, fmt.Errorf("archive %s exceeds the %d byte metadata limit", name, tailWindowLarge)
			}
		default:
			if !metadata.configCandidate(name) {
				continue
			}
			if header.Size > tailWindowLarge || configBytes+header.Size > 8*tailWindowLarge {
				continue
			}
			configBytes += header.Size
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return metadata.contents(), err
		}
		if metadata.record(name, data) && len(metadata.docker) > 0 {
			result := metadata.contents()
			if len(result.RepoTags) > 0 && len(result.MetadataByTag) == len(result.RepoTags) {
				return result, nil
			}
		}
	}
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
