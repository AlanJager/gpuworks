package selfinstall

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// ErrNoTerminal is returned when a prompt is required but nothing can answer
// it. Callers turn it into a message naming the flag that removes the prompt.
var ErrNoTerminal = errors.New("stdin is not a terminal; pass the corresponding flag instead")

// IO owns the process streams. Prompts are only attempted when stdin is a
// terminal, so a piped or unattended run never blocks waiting for input.
type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
	// IsTerminal overrides terminal detection (tests, and callers that know
	// stdin is a pipe even when it happens to be a character device).
	IsTerminal func() bool
	// Quiet silences ordinary output, so an unattended run reports only what
	// went wrong (Warn still writes). It is set from `upgrade --quiet`; install
	// keeps its own --quiet meaning "never prompt" and still prints the summary,
	// because that summary carries the password of a fresh installation.
	Quiet bool

	reader *bufio.Reader
}

// NewIO builds the production IO around the process streams.
func NewIO() *IO {
	return &IO{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}

// Terminal reports whether prompts can be answered.
func (o *IO) Terminal() bool {
	if o == nil {
		return false
	}
	if o.IsTerminal != nil {
		return o.IsTerminal()
	}
	f, ok := o.In.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// Say writes a line of ordinary output.
func (o *IO) Say(format string, args ...any) {
	if o == nil || o.Quiet || o.Out == nil {
		return
	}
	fmt.Fprintf(o.Out, format+"\n", args...)
}

// Warn writes a line to stderr.
func (o *IO) Warn(format string, args ...any) {
	if o == nil || o.Err == nil {
		return
	}
	fmt.Fprintf(o.Err, format+"\n", args...)
}

func (o *IO) line() *bufio.Reader {
	if o.reader == nil {
		in := o.In
		if in == nil {
			in = strings.NewReader("")
		}
		o.reader = bufio.NewReader(in)
	}
	return o.reader
}

// Prompt asks a question and returns the trimmed answer. An empty answer is
// returned as-is; callers decide what it means.
func (o *IO) Prompt(question string) (string, error) {
	if !o.Terminal() {
		return "", ErrNoTerminal
	}
	fmt.Fprintf(o.Out, "%s", question)
	answer, err := o.line().ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(answer), nil
}

// PromptPassword asks a question without echoing the answer. Without a file
// descriptor to turn the echo off on — an injected terminal, in tests — it
// falls back to an ordinary prompt rather than refusing to ask.
func (o *IO) PromptPassword(question string) (string, error) {
	if !o.Terminal() {
		return "", ErrNoTerminal
	}
	f, ok := o.In.(*os.File)
	if !ok {
		return o.Prompt(question)
	}
	fmt.Fprintf(o.Out, "%s", question)
	raw, err := term.ReadPassword(int(f.Fd()))
	fmt.Fprintln(o.Out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// Confirm asks a yes/no question. A blank answer takes want.
func (o *IO) Confirm(question string, want bool) (bool, error) {
	suffix := " [y/N] "
	if want {
		suffix = " [Y/n] "
	}
	answer, err := o.Prompt(question + suffix)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(answer) {
	case "":
		return want, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return false, fmt.Errorf("please answer y or n (got %q)", answer)
	}
}
