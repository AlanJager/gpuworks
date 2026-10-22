package modelfetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestProbeTellsUsableFromNeedsALoginFromBroken(t *testing.T) {
	// As Hugging Face answers: a gated repository's record is public and its
	// files are refused without a granted token (hf-mirror says 403, the hub
	// 401); a private repository refuses its record too. Files are served as
	// a Range request for one byte is.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Millisecond) // every answer takes a measurable time
		switch r.URL.Path {
		case "/api/models/org/open/revision/main", "/api/v1/models/org/open", "/api/models/org/gated/revision/main", "/api/models/org/gguf/revision/main":
			_, _ = w.Write([]byte(`{"sha":"abc"}`))
		case "/org/open/resolve/main/config.json":
			if r.Header.Get("Range") != "bytes=0-0" {
				t.Errorf("file asked without a one-byte range: %q", r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 0-0/2")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("{"))
		case "/org/gated/resolve/main/config.json":
			http.Error(w, "Access to model org/gated is restricted", http.StatusForbidden)
		case "/api/models/org/private/revision/main":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			http.Error(w, "missing", http.StatusNotFound)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		source, repo, base string
		status             string
		code               int
	}{
		"hugging face":          {modelrepo.SourceHF, "org/open", server.URL, proto.SourceProbeOK, 200},
		"modelscope":            {modelrepo.SourceMS, "org/open", server.URL, proto.SourceProbeOK, 200},
		"gated":                 {modelrepo.SourceHF, "org/gated", server.URL, proto.SourceProbeAuthRequired, 403},
		"private":               {modelrepo.SourceHF, "org/private", server.URL, proto.SourceProbeAuthRequired, 401},
		"no config.json":        {modelrepo.SourceHF, "org/gguf", server.URL, proto.SourceProbeOK, 200},
		"missing":               {modelrepo.SourceHF, "org/missing", server.URL, proto.SourceProbeHTTPError, 404},
		"nothing listens there": {modelrepo.SourceHF, "org/open", deadAddress(t), proto.SourceProbeUnreachable, 0},
	} {
		got := Probe(ctx, tc.source, tc.repo, tc.base, nil, nil)
		if got.Status != tc.status || got.Code != tc.code || got.Base != tc.base {
			t.Errorf("%s: %+v, want %s/%d", name, got, tc.status, tc.code)
		}
		if tc.status != proto.SourceProbeUnreachable && got.LatencyMS < 5 {
			t.Errorf("%s: latency %d ms, want the time the address took", name, got.LatencyMS)
		}
		if tc.status == proto.SourceProbeUnreachable && got.Error == "" {
			t.Errorf("%s: no error said why", name)
		}
	}
}
