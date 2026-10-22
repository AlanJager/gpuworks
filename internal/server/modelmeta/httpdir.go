package modelmeta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
	"gpuworks.ai/gpuworks/pkg/safetensors"
)

// SourceURL names a model served by a plain HTTP directory index. It is
// re-exported from modelrepo so the API layer names the source without
// depending on the repository package directly.
const SourceURL = modelrepo.SourceURL

// Why a directory could not be turned into a model. Each maps to one product
// error, so the operator is told which part of the request failed rather
// than being handed a transport message.
var (
	// ErrDirectoryURLInvalid rejects an unusable URL before any network request.
	ErrDirectoryURLInvalid = errors.New("model directory URL is invalid")
	// ErrDirectoryUnreachable covers a valid URL that cannot be read at all:
	// a host that does not answer or a page that is not a directory index.
	ErrDirectoryUnreachable = errors.New("http directory is unreachable")
	// ErrDirectoryNotAModel covers a directory that reads fine but lists no
	// file at all, or whose config.json does not parse. A directory without a
	// config.json is still a model - which files it needs is the engine's
	// business, and a GGUF-only repository has none - it only states no
	// metadata.
	ErrDirectoryNotAModel = errors.New("http directory is not a model repository")
	// ErrDirectoryTooLarge covers a tree larger than the listing walk's bounds.
	ErrDirectoryTooLarge = errors.New("http directory is too large to list")
)

// DirectoryPreview is what an operator is shown before a directory URL becomes
// a registered model: the identity that will be written, and enough of the
// model's own facts to recognize that the URL points at the intended weights.
type DirectoryPreview struct {
	SourceURL       string
	SourceID        string
	Name            string
	Arch            string
	Precision       string
	ContextLength   int64
	WeightBytes     int64
	FilesCount      int
	InputModalities []string
	ConfigRaw       string
}

// PreviewDirectory reads a directory URL as a model repository without
// registering anything. Everything it returns is derived from the directory
// itself, so confirming the preview and registering describe the same bytes.
//
// A URL that names a file rather than a directory — the operator copied the
// link to model.safetensors out of the index page — falls back to the
// directory containing it instead of failing, because that is unambiguously
// what was meant.
func PreviewDirectory(ctx context.Context, raw string) (*DirectoryPreview, error) {
	return PreviewDirectoryWithClient(ctx, raw, nil)
}

// PreviewDirectoryWithClient preserves directory redirect confinement while using the supplied transport.
func PreviewDirectoryWithClient(ctx context.Context, raw string, base *http.Client) (*DirectoryPreview, error) {
	normalized, err := modelrepo.NormalizeDirectoryURL(raw)
	if err != nil {
		return nil, ErrDirectoryURLInvalid
	}
	client, err := modelrepo.DirectoryClient(normalized, base)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDirectoryUnreachable, err.Error())
	}
	normalized = directoryOfFileURL(ctx, normalized, client)
	files, err := modelrepo.ListFiles(ctx, modelrepo.SourceURL, "", "", normalized, nil, modelrepo.RequestOptions{Client: client})
	if err != nil {
		if errors.Is(err, modelrepo.ErrListingTooLarge) {
			return nil, fmt.Errorf("%w: %s", ErrDirectoryTooLarge, err.Error())
		}
		if errors.Is(err, modelrepo.ErrEmptyListing) {
			return nil, fmt.Errorf("%w: %s", ErrDirectoryNotAModel, err.Error())
		}
		return nil, fmt.Errorf("%w: %s", ErrDirectoryUnreachable, err.Error())
	}
	var configRaw []byte
	var cfg *modelconfig.Config
	if listingHasFile(files, "config.json") {
		configRaw, err = getWith(ctx, client, modelrepo.ResolveURL(modelrepo.SourceURL, "", "", "config.json", normalized))
		if err != nil {
			return nil, fmt.Errorf("%w: read config.json: %s", ErrDirectoryUnreachable, err.Error())
		}
		cfg, err = modelconfig.Parse(strings.NewReader(string(configRaw)))
		if err != nil {
			return nil, fmt.Errorf("%w: config.json at %s did not parse: %s", ErrDirectoryNotAModel, normalized, err.Error())
		}
	}
	preview := &DirectoryPreview{
		SourceURL:       normalized,
		SourceID:        modelrepo.SuggestRepoID(normalized),
		Name:            lastURLSegment(normalized),
		Arch:            archOf(cfg),
		Precision:       Precision(cfg, string(configRaw)),
		ContextLength:   ContextLength(cfg, string(configRaw)),
		FilesCount:      len(files),
		InputModalities: InputModalities(cfg),
		ConfigRaw:       string(configRaw),
	}
	preview.WeightBytes = directoryWeightBytes(ctx, client, normalized, files)
	return preview, nil
}

// directoryWeightBytes reports the model's weight size as exactly as the
// directory allows: the shard index carries a real total, and a single-file
// model is worth one HEAD. Otherwise the listing's rounded sizes stand, which
// is the deliberate trade for not issuing one request per shard.
func directoryWeightBytes(ctx context.Context, client *http.Client, directory string, files []modelrepo.File) int64 {
	if listingHasFile(files, "model.safetensors.index.json") {
		body, err := getWith(ctx, client, modelrepo.ResolveURL(modelrepo.SourceURL, "", "", "model.safetensors.index.json", directory))
		if err == nil {
			if info, perr := safetensors.ParseIndexJSON(strings.NewReader(string(body))); perr == nil && info.TotalBytes > 0 {
				return info.TotalBytes
			}
		}
	}
	selected := selectWeightFiles(files)
	var total int64
	for _, file := range selected {
		total += file.Size
	}
	return exactListingWeightBytes(ctx, client, modelrepo.SourceURL, "", directory, "", selected, total)
}

// exactListingWeightBytes upgrades a rounded listing total to the exact byte
// count when one request can do it. A single-file model is the common case the
// shard index does not cover, and its weight size drives the whole VRAM
// estimate, so it is worth the one HEAD; a sharded model is left approximate
// rather than spending one request per shard.
func exactListingWeightBytes(ctx context.Context, client *http.Client, source, revision, endpoint, repo string, selected []modelrepo.File, total int64) int64 {
	if !modelrepo.SizeIsApproximate(source) || len(selected) != 1 {
		return total
	}
	fileURL := modelrepo.ResolveURL(source, repo, revision, selected[0].Path, endpoint)
	exact, _, err := modelrepo.HeadFile(ctx, fileURL, client)
	if err != nil || exact <= 0 {
		return total
	}
	return exact
}

// sourceClient returns the HTTP client a source's metadata requests go
// through. Directory requests additionally remain confined to the registered
// host; repository APIs use the supplied client unchanged.
func sourceClient(source, endpoint string, base *http.Client) (*http.Client, error) {
	if source != modelrepo.SourceURL {
		return base, nil
	}
	return modelrepo.DirectoryClient(endpoint, base)
}

// directoryOfFileURL returns the directory a URL describes. A URL whose last
// segment looks like a file name is confirmed with one HEAD before its parent
// is used: a repository legitimately named "Qwen3-0.6B-v1.5" must not be
// mistaken for a file, and a server that will not answer the HEAD is left to
// the listing to report.
func directoryOfFileURL(ctx context.Context, normalized string, client *http.Client) string {
	segment := lastURLSegment(normalized)
	if !looksLikeFileName(segment) {
		return normalized
	}
	parent := strings.TrimSuffix(normalized, "/"+segment)
	if parent == normalized || parent == "" {
		return normalized
	}
	_, contentType, err := modelrepo.HeadFile(ctx, normalized, client)
	if err != nil || contentType == "" || strings.Contains(contentType, "html") {
		return normalized
	}
	return parent
}

// looksLikeFileName reports whether a path segment ends in a file extension.
//
// A dot alone does not make one: repository names carry sizes and versions
// ("Qwen3-0.6B", "Llama-3.3-70B"), and probing every such directory as a file
// would cost an extra request on the common case. A real extension is letters
// only — .json, .safetensors, .bin, .gguf — so a suffix containing a digit is
// part of the name.
func looksLikeFileName(segment string) bool {
	extension := strings.TrimPrefix(path.Ext(segment), ".")
	if len(extension) < 2 || len(extension) > 12 {
		return false
	}
	for _, char := range extension {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') {
			return false
		}
	}
	return true
}

func listingHasFile(files []modelrepo.File, name string) bool {
	for _, file := range files {
		if file.Path == name {
			return true
		}
	}
	return false
}

// lastURLSegment is the directory's own name, which is what an operator
// recognizes the model by. A URL with no path at all has no such name.
func lastURLSegment(normalized string) string {
	parsed, err := url.Parse(normalized)
	if err != nil {
		return ""
	}
	trimmed := strings.TrimRight(parsed.Path, "/")
	if index := strings.LastIndex(trimmed, "/"); index >= 0 {
		return trimmed[index+1:]
	}
	return trimmed
}
