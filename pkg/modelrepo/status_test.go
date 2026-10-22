package modelrepo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBaseURLIsWhereEachSourceLives(t *testing.T) {
	for name, tc := range map[string]struct {
		source, sourceURL, want string
	}{
		"hugging face":          {SourceHF, "", "https://huggingface.co"},
		"modelscope":            {SourceMS, "ignored", "https://modelscope.cn"},
		"directory":             {SourceURL, " http://files.example/models/qwen/ ", "http://files.example/models/qwen"},
		"unknown":               {"local", "", ""},
		"directory without url": {SourceURL, "", ""},
	} {
		if got := BaseURL(tc.source, tc.sourceURL); got != tc.want {
			t.Errorf("%s: BaseURL = %q, want %q", name, got, tc.want)
		}
	}
}

// A hub that answers with an error status yields a *StatusError carrying the
// status, so a caller walking several addresses can tell a mirror that lacks
// the repository from a broken connection without reading message text.
func TestHubErrorStatusesAreTyped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/models/gated/"):
			// Hugging Face's gated-repository answer.
			w.Header().Set("X-Error-Code", "GatedRepo")
			http.Error(w, `{"error":"Access to model gated/model is restricted."}`, http.StatusUnauthorized)
		case strings.HasPrefix(r.URL.Path, "/api/models/"):
			http.Error(w, `{"error":"Repository not found"}`, http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/api/v1/models/"):
			// ModelScope reports a missing model inside a 200 response.
			_, _ = w.Write([]byte(`{"Code":404,"Message":"The model can not be found","Success":false,"Data":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	for name, tc := range map[string]struct {
		call func() error
		code int
		text string
	}{
		"hf missing repository": {func() error {
			_, err := ListFiles(ctx, SourceHF, "org/missing", "main", server.URL, nil)
			return err
		}, http.StatusNotFound, "http 404"},
		"hf gated revision": {func() error {
			_, err := ResolveRevision(ctx, SourceHF, "gated/model", "main", server.URL, nil)
			return err
		}, http.StatusUnauthorized, "http 401"},
		"modelscope envelope": {func() error {
			_, err := ListFiles(ctx, SourceMS, "org/missing", "", server.URL, nil)
			return err
		}, http.StatusNotFound, "message=The model can not be found"},
	} {
		err := tc.call()
		var status *StatusError
		if !errors.As(err, &status) || status.Code != tc.code {
			t.Errorf("%s: err = %v, want a StatusError with code %d", name, err, tc.code)
			continue
		}
		if !strings.Contains(err.Error(), tc.text) || !strings.Contains(status.URL, server.URL) {
			t.Errorf("%s: err = %q (url %q), want it to mention %q and the address", name, err, status.URL, tc.text)
		}
	}
}
