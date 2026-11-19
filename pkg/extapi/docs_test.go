package extapi

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// referencePages document the wire formats. Every JSON example on them is a
// copy of a golden file, named in a comment on the line before its fence, so
// the pages cannot drift from the frozen corpus.
var referencePages = []string{
	"../../docs/reference/plugin-protocol.md",
	"../../docs-en/reference/plugin-protocol.md",
}

var goldenMarker = regexp.MustCompile(`^<!-- golden: ([a-z_]+/v[1-9][0-9]*/(?:(?:valid|invalid/[A-Z_]+)/[a-z0-9_.-]+\.json|signed/[a-z0-9_.-]+\.txt)) -->$`)

// embeddedExample is one JSON example on a reference page.
type embeddedExample struct {
	golden string // path below goldenRoot
	body   string // the fence's contents, lines joined by "\n"
	line   int    // 1-based line of the marker
}

// TestReferencePageExamplesAreGoldenFiles keeps the plugin protocol
// reference honest: each embedded example must equal its golden file byte
// for byte, every JSON block must name one, both languages embed the same
// examples, and every wire object has at least one.
//
//	cd pkg/extapi && go test -run TestReferencePageExamplesAreGoldenFiles .
func TestReferencePageExamplesAreGoldenFiles(t *testing.T) {
	if _, err := os.Stat(referencePages[0]); errors.Is(err, fs.ErrNotExist) && os.Getenv("GPUWORKS_TEST_STRICT") != "1" {
		t.Skip("the module is outside its repository, so the reference pages are not here")
	}
	var first []string
	for i, page := range referencePages {
		examples := embeddedExamples(t, page)
		covered := map[string]bool{}
		var names []string
		for _, ex := range examples {
			names = append(names, ex.golden)
			covered[strings.SplitN(ex.golden, "/", 2)[0]] = true
			want, err := os.ReadFile(filepath.Join(goldenRoot, filepath.FromSlash(ex.golden)))
			if err != nil {
				t.Errorf("%s:%d: %v", page, ex.line, err)
				continue
			}
			if ex.body != string(want) {
				t.Errorf("%s:%d: the example differs from golden %s; paste the file unchanged\n got %s\nwant %s",
					page, ex.line, ex.golden, ex.body, want)
			}
		}
		for kind := range goldenDecoders {
			if !covered[kind] {
				t.Errorf("%s: no example of %s; embed one of %s", page, kind, path.Join(goldenRoot, kind, "v1", "valid"))
			}
		}
		if i == 0 {
			first = names
		} else if !reflect.DeepEqual(names, first) {
			t.Errorf("%s embeds %v, but %s embeds %v", page, names, referencePages[0], first)
		}
	}
}

// embeddedExamples reads the golden examples of a page. A "```json" fence
// must follow a golden marker line directly, and a marker must be followed
// by such a fence, or by a "```text" fence for a signed .txt vector.
func embeddedExamples(t *testing.T, page string) []embeddedExample {
	t.Helper()
	raw, err := os.ReadFile(page)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	var out []embeddedExample
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "```json") {
			t.Errorf("%s:%d: a JSON example without a golden marker on the line before it", page, i+1)
			continue
		}
		if strings.HasPrefix(line, "<!-- golden:") {
			m := goldenMarker.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%s:%d: malformed golden marker %q", page, i+1, line)
				continue
			}
			// A text golden file (signed headers) ends with a newline the
			// fence does not show.
			fence, tail := "```json", ""
			if strings.HasSuffix(m[1], ".txt") {
				fence, tail = "```text", "\n"
			}
			if i+1 >= len(lines) || lines[i+1] != fence {
				t.Errorf("%s:%d: a golden marker must be followed directly by a %s fence", page, i+1, fence)
				continue
			}
			end := i + 2
			for end < len(lines) && lines[end] != "```" {
				end++
			}
			if end == len(lines) {
				t.Fatalf("%s:%d: unclosed fence", page, i+2)
			}
			out = append(out, embeddedExample{golden: m[1], body: strings.Join(lines[i+2:end], "\n") + tail, line: i + 1})
			i = end
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: no golden examples", page)
	}
	return out
}
