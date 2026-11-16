package cli

import (
	"io"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/version"
)

// runStdout runs one subcommand and returns what it printed on stdout.
func runStdout(t *testing.T, args []string, opt Options) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	code := Run(discardLog(), args, io.Discard, opt)
	os.Stdout = stdout
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out), code
}

// keepEdition restores version.Edition after a test that sets it through
// Options, so later tests see the community default.
func keepEdition(t *testing.T) {
	t.Helper()
	saved := version.Edition
	t.Cleanup(func() { version.Edition = saved })
}

func TestEditionCommandPrintsTheCommunityEditionByDefault(t *testing.T) {
	keepEdition(t)
	out, code := runStdout(t, []string{"edition"}, Options{})
	if code != 0 || out != "community\n" {
		t.Fatalf("exit %d, output %q; want community", code, out)
	}
}

// The edition an Options names is written before dispatch, so the edition
// subcommand (and the server serve starts) report it.
func TestOptionsEditionIsWrittenBeforeDispatch(t *testing.T) {
	keepEdition(t)
	out, code := runStdout(t, []string{"edition"}, Options{Edition: "oem-x"})
	if code != 0 || out != "oem-x\n" {
		t.Fatalf("exit %d, output %q; want oem-x", code, out)
	}
	if version.Edition != "oem-x" {
		t.Fatalf("version.Edition = %q after Run", version.Edition)
	}
}

// selfinstall parses the version line of an installed or staged binary, so
// the edition must never appear in it.
func TestVersionLineIsTheSameInEveryEdition(t *testing.T) {
	keepEdition(t)
	want := "gpuworks " + version.String() + "\n"
	for _, opt := range []Options{{}, {Edition: "oem-x"}} {
		out, code := runStdout(t, []string{"version"}, opt)
		if code != 0 || out != want {
			t.Fatalf("edition %q: exit %d, version line %q, want %q", opt.Edition, code, out, want)
		}
	}
}

func TestRunPanicsOnAMalformedEdition(t *testing.T) {
	keepEdition(t)
	for _, edition := range []string{"Community", "has space", "-lead", strings.Repeat("x", 33)} {
		t.Run(edition, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("Run accepted edition %q", edition)
				}
				if version.Edition != "community" {
					t.Fatalf("a refused edition was written: %q", version.Edition)
				}
			}()
			Run(discardLog(), []string{"edition"}, io.Discard, Options{Edition: edition})
		})
	}
}
