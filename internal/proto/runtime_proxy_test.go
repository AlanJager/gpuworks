package proto

import "testing"

// docker info masks any user information of a proxy URL as xxxxx:xxxxx; the
// platform compares what it set with what the daemon reports in that form.
func TestMaskProxyCredentialsMatchesDockerInfo(t *testing.T) {
	for in, want := range map[string]string{
		" http://user:pw@proxy.corp:3128 ": "http://xxxxx:xxxxx@proxy.corp:3128",
		"http://user@proxy.corp:3128":      "http://xxxxx:xxxxx@proxy.corp:3128",
		"http://proxy.corp:3128":           "http://proxy.corp:3128",
		"":                                 "",
		"::not a url":                      "::not a url",
	} {
		if got := MaskProxyCredentials(in); got != want {
			t.Errorf("MaskProxyCredentials(%q) = %q, want %q", in, got, want)
		}
	}
}
