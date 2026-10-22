//go:build cloudgpu && partition

package l4

import "testing"

// The discovered card feeds ASCEND_NERDCTL_VERIFY_INDEXES verbatim, so the
// parse must accept the preflight's real output shape and reject anything a
// remote banner noise line could smuggle in.
func TestParseAscendPreflightCard(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"preflight output", "Authorized users only.\nASCEND_CARD=1\n", "1"},
		{"first card wins", "ASCEND_CARD=7\nASCEND_CARD=3\n", "7"},
		{"non-numeric is rejected", "ASCEND_CARD=three\n", ""},
		{"banner noise only", "Authorized users only. All activities may be monitored.\n", ""},
		{"empty", "", ""},
	} {
		if got := parseAscendPreflightCard(tc.out); got != tc.want {
			t.Errorf("%s: parseAscendPreflightCard(%q) = %q, want %q", tc.name, tc.out, got, tc.want)
		}
	}
}
