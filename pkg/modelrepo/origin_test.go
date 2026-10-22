package modelrepo

import "testing"

// The host a download comes from: the source's own site, an endpoint that
// replaces it, a URL model's own server, and a mirror the caller's rewrite
// puts in front of the site.
func TestOriginIsTheHostTheDownloadComesFrom(t *testing.T) {
	mirror := RequestOptions{Rewrite: func(u string) string {
		if u == "https://huggingface.co" {
			return "https://hf-mirror.com"
		}
		return u
	}}
	for name, tc := range map[string]struct {
		source, endpoint string
		options          []RequestOptions
		want             string
	}{
		"hugging face":          {SourceHF, "", nil, "huggingface.co"},
		"modelscope":            {SourceMS, "", nil, "modelscope.cn"},
		"explicit endpoint":     {SourceHF, "https://hf.corp.example:8443/", nil, "hf.corp.example:8443"},
		"url model":             {SourceURL, "http://files.example/models/qwen/", nil, "files.example"},
		"rewritten to a mirror": {SourceHF, "", []RequestOptions{mirror}, "hf-mirror.com"},
		"rewrite elsewhere":     {SourceMS, "", []RequestOptions{mirror}, "modelscope.cn"},
		"unknown source":        {"local", "", nil, ""},
	} {
		if got := Origin(tc.source, tc.endpoint, tc.options...); got != tc.want {
			t.Errorf("%s: Origin = %q, want %q", name, got, tc.want)
		}
	}
}
