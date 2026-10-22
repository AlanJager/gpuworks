package modelfetch

import (
	"context"
	"io"
	"net/http"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// ProbeTimeout bounds asking one address about a repository.
const ProbeTimeout = 10 * time.Second

// Probe asks base whether it serves repo, through client - the client a
// download from base would use - and with the login a download would carry.
// A 401 or 403 says a login is needed (a gated repository, a mirror that
// takes none) rather than that the address is unusable. On a Hugging
// Face-style address a repository that answers is asked for one byte of
// config.json too: a gated repository refuses only its files. A file it
// lacks (a GGUF repository has no config.json) changes nothing.
func Probe(ctx context.Context, source, repo, base string, client *http.Client, logins []modelrepo.Login) (out proto.SourceProbeOutcome) {
	out.Base = base
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	if client == nil {
		client = http.DefaultClient
	}
	started := time.Now()
	defer func() { out.LatencyMS = time.Since(started).Milliseconds() }()
	code, err := ask(ctx, client, modelrepo.ProbeURL(source, repo, base), logins)
	if err != nil {
		out.Status, out.Error = proto.SourceProbeUnreachable, err.Error()
		return out
	}
	out.Code, out.Status = code, probeStatus(code)
	if out.Status != proto.SourceProbeOK {
		return out
	}
	if file := modelrepo.ProbeFileURL(source, repo, base); file != "" {
		if code, err := ask(ctx, client, file, logins); err == nil && probeStatus(code) == proto.SourceProbeAuthRequired {
			out.Code, out.Status = code, proto.SourceProbeAuthRequired
		}
	}
	return out
}

func probeStatus(code int) string {
	switch {
	case code == http.StatusOK || code == http.StatusPartialContent:
		return proto.SourceProbeOK
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return proto.SourceProbeAuthRequired
	default:
		return proto.SourceProbeHTTPError
	}
}

// ask GETs target (its first byte only) and answers its status.
func ask(ctx context.Context, client *http.Client, target string, logins []modelrepo.Login) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	for key, value := range modelrepo.AuthorizationHeaders(target, logins) {
		req.Header.Set(key, value)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}
