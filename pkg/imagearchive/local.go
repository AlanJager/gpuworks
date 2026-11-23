package imagearchive

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
)

var errNoManifest = errors.New("tar contains no Docker or OCI image manifest")

// ReadRepoTags returns the archive's tagged references.
func ReadRepoTags(ctx context.Context, file io.ReadSeeker) ([]string, error) {
	contents, err := ReadContents(ctx, file)
	if errors.Is(err, errNoManifest) {
		return nil, nil
	}
	return contents.RepoTags, err
}

// ReadContents reads top-level image metadata. With an uncompressed tar the
// tar reader seeks over layers, even when manifest.json is the last member.
func ReadContents(ctx context.Context, file io.ReadSeeker) (Contents, error) {
	var magic [2]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return Contents{}, err
	}
	var metadata archiveMetadata
	scan := func(wanted map[string]bool) (Contents, error) {
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
		return metadata.readTar(ctx, input, wanted)
	}
	result, err := scan(nil)
	if err == nil && len(metadata.docker) == 0 && len(metadata.oci) == 0 {
		return Contents{}, errNoManifest
	}
	if err != nil || !metadata.budgetLimited || len(result.RepoTags) == len(result.MetadataByTag) {
		return result, err
	}
	// Candidate blobs before the manifest may be layers. Revisit only the
	// files named by the manifest, still under the same per-pass byte limit.
	if len(metadata.dockerConfigs) > 0 {
		return scan(metadata.dockerConfigs)
	}
	manifests := make(map[string]bool)
	for _, desc := range metadata.oci {
		if desc.Annotations[refNameAnnotation] != "" {
			manifests[blobPath(desc.Digest)] = true
		}
	}
	if len(manifests) == 0 {
		return result, nil
	}
	if _, err := scan(manifests); err != nil {
		return Contents{}, err
	}
	configs := make(map[string]bool)
	for path := range manifests {
		if config := metadata.configPaths[path]; config != "" {
			configs[config] = true
		}
	}
	return scan(configs)
}

func (metadata *archiveMetadata) readTar(ctx context.Context, input io.Reader, wanted map[string]bool) (Contents, error) {
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
			if (wanted != nil && !wanted[name]) || !metadata.configCandidate(name) {
				continue
			}
			if header.Size > tailWindowLarge {
				continue
			}
			if configBytes+header.Size > 8*tailWindowLarge {
				metadata.budgetLimited = true
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
