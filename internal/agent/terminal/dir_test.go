//go:build linux || darwin

package terminal

import (
	"testing"
	"time"
)

// Spec.Dir overrides the home directory the shell starts in; the container
// branch uses it so a terminal never starts in a directory (/root) that the
// runtime's account cannot enter.
func TestSessionDirOverridesHome(t *testing.T) {
	s, err := Open(Spec{Dir: "/"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if s.cmd.Dir != "/" {
		t.Fatalf("cmd.Dir = %q, want /", s.cmd.Dir)
	}
	if _, err := s.Write([]byte("printf 'cwd|%s|\\n' \"$(pwd)\"\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, s, "cwd|/|", 10*time.Second)
}
