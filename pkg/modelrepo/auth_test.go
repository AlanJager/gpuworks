package modelrepo

import (
	"encoding/base64"
	"testing"
)

func TestAuthorizationGoesToItsOwnHostOverHTTPSOnly(t *testing.T) {
	logins := []Login{
		{Host: "huggingface.co", Secret: "hf_token"},
		{Host: "Mirror.Corp:8443", Username: "alice", Secret: "pw"},
		{Host: "empty.example", Username: "bob"},
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:pw"))
	for rawURL, want := range map[string]string{
		"https://huggingface.co/api/models/org/m/revision/main":  "Bearer hf_token",
		"https://huggingface.co:443/org/m/resolve/x/config.json": "Bearer hf_token",
		"https://HUGGINGFACE.CO/org/m":                           "Bearer hf_token",
		"https://hf-mirror.com/org/m/resolve/x/config.json":      "",
		"https://cdn-lfs.huggingface.co/blob":                    "",
		"http://huggingface.co/org/m":                            "",
		"https://mirror.corp:8443/hf/org/m":                      basic,
		"https://mirror.corp/hf/org/m":                           "",
		"https://empty.example/x":                                "",
		"not a url":                                              "",
	} {
		if got := AuthorizationFor(rawURL, logins); got != want {
			t.Errorf("AuthorizationFor(%q) = %q, want %q", rawURL, got, want)
		}
	}
	if AuthorizationHeaders("https://hf-mirror.com/x", logins) != nil {
		t.Error("no login, no headers")
	}
	if h := AuthorizationHeaders("https://huggingface.co/x", logins); h["Authorization"] != "Bearer hf_token" {
		t.Errorf("headers = %v", h)
	}
}
