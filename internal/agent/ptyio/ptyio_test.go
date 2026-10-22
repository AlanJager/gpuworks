//go:build linux || darwin

package ptyio

import (
	"io"
	"strings"
	"testing"
)

// What a child writes to the slave arrives on the master with the terminal's
// line discipline applied, the size set on the master is the size the child
// reads, and the end of the output once every slave fd is closed is IsClosed.
func TestOpenCarriesOutputSizeAndEnd(t *testing.T) {
	master, slave, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := SetSize(master, 50, 200); err != nil {
		t.Fatal(err)
	}
	if rows, cols, err := Size(master); err != nil || rows != 50 || cols != 200 {
		t.Fatalf("size = %dx%d err=%v", rows, cols, err)
	}
	if _, err := slave.WriteString("layer: Downloading 1.0MB/2.0MB\n"); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	out, err := io.ReadAll(master)
	if !strings.Contains(string(out), "layer: Downloading 1.0MB/2.0MB\r\n") {
		t.Fatalf("output = %q, want the line with the terminal's CRLF", out)
	}
	if err == nil || !IsClosed(err) {
		t.Fatalf("end of output = %v, want the closed-terminal EIO", err)
	}
}

func TestIsClosedOnlyMatchesTheTerminalEnd(t *testing.T) {
	if IsClosed(io.EOF) || IsClosed(nil) {
		t.Fatal("only EIO is the end of a terminal's output")
	}
}
