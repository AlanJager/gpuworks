package tokenizer

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// LoadRef loads a tokenizer from a local path, an HTTP(S) URL, or a compact
// repository reference (hf://org/model[@revision], ms://org/model[@revision]).
// Valid HTTP downloads are reused for up to 24 hours from the OS user cache
// directory (gpuworks/tokenizers); local files are always read directly.
// It deliberately returns an error rather than hiding failures so callers can
// decide whether estimation is allowed for the workload without hiding failures.
func LoadRef(ctx context.Context, ref string) (*Tokenizer, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty tokenizer reference")
	}
	if strings.HasPrefix(ref, "hf://") || strings.HasPrefix(ref, "ms://") {
		return loadRepoRef(ctx, ref)
	}
	if u, err := url.Parse(ref); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return loadHTTP(ctx, ref)
	}
	f, err := os.Open(ref)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return LoadFile(f)
}

func loadRepoRef(ctx context.Context, ref string) (*Tokenizer, error) {
	source := modelrepo.SourceHF
	prefix := "hf://"
	if strings.HasPrefix(ref, "ms://") {
		source = modelrepo.SourceMS
		prefix = "ms://"
	}
	value := strings.TrimPrefix(ref, prefix)
	parts := strings.SplitN(value, "@", 2)
	repo := strings.Trim(parts[0], "/")
	if repo == "" {
		return nil, fmt.Errorf("tokenizer repository is empty")
	}
	revision := "main"
	if len(parts) == 2 && strings.TrimSpace(parts[1]) != "" {
		revision = strings.TrimSpace(parts[1])
	}
	tok, err := loadHTTP(ctx, modelrepo.ResolveURL(source, repo, revision, "tokenizer.json", ""))
	if errors.Is(err, errNotFound) {
		// tiktoken-based models ship their vocabulary instead.
		if tiktoken, tiktokenErr := loadHTTP(ctx, modelrepo.ResolveURL(source, repo, revision, TiktokenFile, "")); tiktokenErr == nil {
			return tiktoken, nil
		}
	}
	return tok, err
}
