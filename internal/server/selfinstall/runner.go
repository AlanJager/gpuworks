package selfinstall

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Command is one external command. Sudo asks for privilege escalation; the
// runner satisfies it by running as root directly or through sudo, which
// prompts on the terminal for the first privileged command and then reuses
// its cached ticket.
type Command struct {
	Path   string
	Args   []string
	Sudo   bool
	Dir    string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// String renders the command the way an operator would type it, for logs and
// error messages.
func (c Command) String() string {
	parts := make([]string, 0, len(c.Args)+2)
	if c.Sudo {
		parts = append(parts, "sudo")
	}
	parts = append(parts, c.Path)
	parts = append(parts, c.Args...)
	return strings.Join(parts, " ")
}

// Runner executes commands. Tests inject a recording runner; production uses
// ExecRunner.
type Runner interface {
	Run(ctx context.Context, cmd Command) error
	Output(ctx context.Context, cmd Command) (string, error)
}

// ExecRunner runs commands as subprocesses.
type ExecRunner struct {
	// Euid reports the effective uid; nil means os.Geteuid. Tests set it to
	// exercise the sudo path without being root.
	Euid func() int
	// Stderr receives command output when a Command carries no explicit writer,
	// so the operator sees what the install is doing.
	Stderr io.Writer
}

// Run executes cmd and discards its stdout.
func (r ExecRunner) Run(ctx context.Context, cmd Command) error {
	out := cmd.Stdout
	if out == nil {
		out = io.Discard
	}
	return r.exec(ctx, cmd, out, r.stderr(cmd))
}

// Output executes cmd and returns its trimmed stdout.
func (r ExecRunner) Output(ctx context.Context, cmd Command) (string, error) {
	var buf bytes.Buffer
	if err := r.exec(ctx, cmd, &buf, r.stderr(cmd)); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func (r ExecRunner) stderr(cmd Command) io.Writer {
	if cmd.Stderr != nil {
		return cmd.Stderr
	}
	if r.Stderr != nil {
		return r.Stderr
	}
	return io.Discard
}

func (r ExecRunner) euid() int {
	if r.Euid != nil {
		return r.Euid()
	}
	return os.Geteuid()
}

func (r ExecRunner) exec(ctx context.Context, cmd Command, stdout, stderr io.Writer) error {
	if strings.TrimSpace(cmd.Path) == "" {
		return fmt.Errorf("empty command path")
	}
	path, args := cmd.Path, cmd.Args
	if cmd.Sudo && r.euid() != 0 {
		// Plain sudo: it prompts on the operator's terminal when there is no
		// cached ticket and fails on its own without one, so an unattended run
		// errors loudly instead of hanging.
		args = append([]string{path}, args...)
		path = "sudo"
	}
	c := exec.CommandContext(ctx, path, args...)
	c.Dir = cmd.Dir
	c.Env = append(os.Environ(), cmd.Env...)
	c.Stdin = cmd.Stdin
	c.Stdout = stdout
	c.Stderr = stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", cmd, err)
	}
	return nil
}

// hasBinary reports whether name resolves on PATH.
func hasBinary(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
