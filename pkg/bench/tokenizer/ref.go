package tokenizer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// LoadRef loads a tokenizer from a local path, an HTTP(S) URL, or a compact
// repository reference (hf://org/model[@revision], ms://org/model[@revision]).
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
	return Load(f)
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
	return loadHTTP(ctx, modelrepo.ResolveURL(source, repo, revision, "tokenizer.json", ""))
}

func loadHTTP(parent context.Context, rawURL string) (*Tokenizer, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("tokenizer download returned %s", resp.Status)
	}
	return Load(io.LimitReader(resp.Body, 64<<20))
}
