package modelrepo

import (
	"strings"
	"testing"
)

// A size column is written for humans. Every value the three servers can print
// has to map to a number the disk-admission check can use, and everything else
// has to map to "unknown" rather than to a wrong number or a panic.
func TestParseApproxSizeReadsHumanReadableSizeColumns(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  int64
	}{
		{value: "726", want: 726},
		{value: "9.5K", want: 9728},
		{value: "11M", want: 11534336},
		{value: "1.4G", want: 1503238553},
		{value: "1.4T", want: 1539316278886},
		{value: "-", want: 0},
		{value: "", want: 0},
		{value: "abc", want: 0},
		{value: "1.4Z", want: 0},
		// Spellings the same three servers also emit.
		{value: "  1.4G  ", want: 1503238553},
		{value: "726 B", want: 726},
		{value: "1.4 GiB", want: 1503238553},
		{value: "9.5 KiB", want: 9728},
		{value: "4.0G", want: 4294967296},
		{value: "1503300328", want: 1503300328},
		{value: "0", want: 0},
		// Malformed cells must be unknown, never a partial reading.
		{value: "—", want: 0},
		{value: "&nbsp;", want: 0},
		{value: "1.2.3G", want: 0},
		{value: "-1G", want: 0},
		{value: "G", want: 0},
		{value: "1e9", want: 0},
		{value: "99999999999999999999G", want: 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			if got := parseApproxSize(tc.value); got != tc.want {
				t.Fatalf("parseApproxSize(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

// The normalized directory URL is a model's stored identity, so two spellings
// of one directory must never produce two different models, and a spelling the
// platform cannot safely fetch must be refused at the point it is entered.
func TestNormalizeDirectoryURLProducesOneSpellingPerDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "trailing slash", raw: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B/", want: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"},
		{name: "no trailing slash", raw: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B", want: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"},
		{name: "repeated trailing slashes", raw: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B///", want: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"},
		{name: "surrounding whitespace", raw: "  https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B/  ", want: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"},
		{name: "sort query string", raw: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B/?C=N;O=D", want: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"},
		{name: "fragment", raw: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B#weights", want: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B"},
		{name: "query and fragment", raw: "https://mirror/models/m?a=1#b", want: "https://mirror/models/m"},
		{name: "scheme case is normalized", raw: "HTTPS://mirror/models/m", want: "https://mirror/models/m"},
		{name: "host case is preserved", raw: "https://HF-Mirror.ZStack.ai/Qwen/Qwen3-0.6B", want: "https://HF-Mirror.ZStack.ai/Qwen/Qwen3-0.6B"},
		{name: "explicit port is preserved", raw: "http://mirror.internal:8081/models/Qwen3-0.6B/", want: "http://mirror.internal:8081/models/Qwen3-0.6B"},
		{name: "ipv6 literal", raw: "http://[2001:db8::1]:8080/models/m/", want: "http://[2001:db8::1]:8080/models/m"},
		{name: "plain http is allowed", raw: "http://10.0.0.5/models/m", want: "http://10.0.0.5/models/m"},
		{name: "host root", raw: "https://mirror/", want: "https://mirror"},
		{name: "percent escapes are preserved", raw: "https://mirror/models/Qwen%203/", want: "https://mirror/models/Qwen%203"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDirectoryURL(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeDirectoryURL(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeDirectoryURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			// Normalization is the identity on its own output, otherwise a
			// stored URL could still drift on a later edit.
			again, err := NormalizeDirectoryURL(got)
			if err != nil || again != got {
				t.Fatalf("renormalizing %q = %q, %v", got, again, err)
			}
		})
	}

	// Both spellings of the same directory are literally one value.
	withSlash, err := NormalizeDirectoryURL("https://mirror/Qwen/Qwen3-0.6B/")
	if err != nil {
		t.Fatal(err)
	}
	withoutSlash, err := NormalizeDirectoryURL("https://mirror/Qwen/Qwen3-0.6B")
	if err != nil {
		t.Fatal(err)
	}
	if withSlash != withoutSlash {
		t.Fatalf("trailing slash changed identity: %q vs %q", withSlash, withoutSlash)
	}
}

func TestNormalizeDirectoryURLRejectsWhatItCannotFetch(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: ""},
		{name: "blank", raw: "   "},
		{name: "ftp scheme", raw: "ftp://mirror/models/m"},
		{name: "file scheme", raw: "file:///srv/models/m"},
		{name: "no scheme", raw: "mirror/models/m"},
		{name: "scheme relative", raw: "//mirror/models/m"},
		{name: "no host", raw: "https:///models/m"},
		{name: "port without host", raw: "http://:8080/models/m"},
		{name: "empty IPv6 host", raw: "https://[]:8080/models/m"},
		// Credentials are refused rather than silently stripped: a stripped
		// credential would turn into an anonymous 403 on a node much later.
		{name: "embedded userinfo", raw: "https://user:secret@mirror/models/m"},
		{name: "embedded user only", raw: "https://user@mirror/models/m"},
		{name: "control character", raw: "https://mirror/models/\x7f\x00m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDirectoryURL(tc.raw)
			if err == nil {
				t.Fatalf("NormalizeDirectoryURL(%q) = %q, want an error", tc.raw, got)
			}
			if got != "" {
				t.Fatalf("a rejected URL still produced %q", got)
			}
		})
	}
	// The secret must not travel in the error text an operator sees or a log
	// records.
	_, err := NormalizeDirectoryURL("https://user:secret@mirror/models/m")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("credential leaked into error: %v", err)
	}
}

// The suggested repository id is what the inference catalog and the template
// matcher key on, so it must be the repository's own two-segment name and must
// never pick up the host or port.
func TestSuggestRepoIDUsesTheLastTwoPathSegments(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{name: "two segments", url: "https://hf-mirror.zstack.ai/Qwen/Qwen3-0.6B", want: "Qwen/Qwen3-0.6B"},
		{name: "one segment", url: "https://hf-mirror.zstack.ai/Qwen3-0.6B", want: "Qwen3-0.6B"},
		{name: "more than two segments keeps the last two", url: "https://mirror/pub/models/Qwen/Qwen3-0.6B", want: "Qwen/Qwen3-0.6B"},
		{name: "trailing slash", url: "https://mirror/Qwen/Qwen3-0.6B/", want: "Qwen/Qwen3-0.6B"},
		{name: "explicit port is not a segment", url: "http://mirror.internal:8081/Qwen/Qwen3-0.6B", want: "Qwen/Qwen3-0.6B"},
		{name: "port with a single segment", url: "http://mirror.internal:8081/Qwen3-0.6B", want: "Qwen3-0.6B"},
		{name: "ipv6 host is not a segment", url: "http://[2001:db8::1]:8080/Qwen/Qwen3-0.6B", want: "Qwen/Qwen3-0.6B"},
		{name: "host root has no repository", url: "https://mirror", want: ""},
		{name: "percent escapes are decoded", url: "https://mirror/Qwen%20Team/Qwen3-0.6B", want: "Qwen Team/Qwen3-0.6B"},
		{name: "empty", url: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SuggestRepoID(tc.url); got != tc.want {
				t.Fatalf("SuggestRepoID(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
