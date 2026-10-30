package selfinstall

import (
	"path/filepath"
	"testing"
)

func TestPlatformDefaultDir(t *testing.T) {
	home := func() string { return "/Users/alice" }
	if got := platformDefaultDir("darwin", home); got != "/Users/alice/gpuworks/server" {
		t.Errorf("darwin default = %q, want the operator's ~/gpuworks/server", got)
	}
	if got := platformDefaultDir("linux", home); got != SystemDir {
		t.Errorf("linux default = %q, want %q", got, SystemDir)
	}
	// Without a home to put it in, a Mac falls back to the system directory
	// rather than to a relative path.
	if got := platformDefaultDir("darwin", func() string { return "" }); got != SystemDir {
		t.Errorf("darwin default without a home = %q, want %q", got, SystemDir)
	}
}

func TestExpandHome(t *testing.T) {
	home := func() string { return "/Users/alice" }
	lookup := func(name string) string {
		if name == "bob" {
			return "/Users/bob"
		}
		return ""
	}
	for _, tc := range []struct{ in, want string }{
		{"~", "/Users/alice"},
		{"~/", "/Users/alice"},
		{"~/gpuworks", "/Users/alice/gpuworks"},
		{"~/gpuworks/server/", "/Users/alice/gpuworks/server"},
		{"~bob/gw", "/Users/bob/gw"},
		{"~bob", "/Users/bob"},
		// An unknown account stays as typed, as a shell leaves it.
		{"~nobody/gw", "~nobody/gw"},
		// Only a leading tilde is special.
		{"/opt/a~b", "/opt/a~b"},
		{"gw/~", "gw/~"},
		{"", ""},
	} {
		if got := expandHome(tc.in, home, lookup); got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := expandHome("~/gw", func() string { return "" }, lookup); got != "~/gw" {
		t.Errorf("without a home, expandHome = %q, want it unchanged", got)
	}
}

// A ~ that reached the installer unexpanded (typed at the prompt, or
// --dir=~/x) lands in the home directory, not in a directory named "~".
func TestNewLayoutExpandsHome(t *testing.T) {
	home := ExpandHome("~")
	if home == "~" {
		t.Skip("no home directory to expand against")
	}
	l := NewLayout("~/gw-server", "~/gw-data", "~/gw-logs")
	if l.Dir != filepath.Join(home, "gw-server") || l.DataDir != filepath.Join(home, "gw-data") || l.LogsDir != filepath.Join(home, "gw-logs") {
		t.Fatalf("layout = %+v, want every directory under %s", l, home)
	}
	if l := NewLayout("~/gw-server", "", ""); l.DataDir != filepath.Join(home, "gw-server", "data") {
		t.Fatalf("data dir = %q, want it inside the expanded install dir", l.DataDir)
	}
}
