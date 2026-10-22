package main

import (
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// The address is the one thing an operator copies out of this command, so it
// gets a frame of its own instead of being one more key/value line. The frame
// is a pair of rules rather than a closed box: no right edge to get out of
// alignment when a terminal has opinions about widths.
const (
	// bannerWidth is how wide the rules are drawn. Content wider than this
	// widens the rules instead of being cut: a quick-tunnel hostname is at most
	// 63 characters and fits, and a longer address is still one line.
	bannerWidth = 71
	// bannerIndent keeps the content off the rules.
	bannerIndent = "  "
)

// bannerRule is the rule character. A terminal gets the double rule; anything
// else gets "=", so a redirect, a log file or a pasted bug report stays plain
// ASCII instead of carrying mojibake out of this command.
func bannerRule(terminal bool) string {
	if terminal {
		return "═"
	}
	return "="
}

// addressBanner renders the address under its own heading, with the one thing
// the operator's provider requires them to know. terminal selects the rule
// character; the layout is the same either way.
func addressBanner(address, note string, terminal bool) string {
	lines := []string{bannerIndent + "PUBLIC ADDRESS", bannerIndent + address}
	// The note is a sentence, not a label: wrap it to the frame, with the
	// continuation lines aligned under it.
	for _, line := range wrapNote(note, bannerWidth-len(bannerIndent)) {
		lines = append(lines, bannerIndent+line)
	}
	width := bannerWidth
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > width {
			width = n
		}
	}
	rule := strings.Repeat(bannerRule(terminal), width)
	return strings.Join(append([]string{rule}, append(lines, rule)...), "\n")
}

// wrapNote breaks a note on spaces so every line fits the frame. A single word
// longer than the frame is left as it is: cutting a word would change what it
// says, and an address in a note is exactly that kind of word.
func wrapNote(note string, width int) []string {
	note = strings.TrimSpace(note)
	if note == "" {
		return nil
	}
	if width <= 0 {
		return []string{note}
	}
	var lines []string
	current := ""
	for _, word := range strings.Fields(note) {
		switch {
		case current == "":
			current = word
		case utf8.RuneCountInString(current)+1+utf8.RuneCountInString(word) <= width:
			current += " " + word
		default:
			lines = append(lines, current)
			current = word
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// printAddressBanner writes the banner as ordinary output. Without an address
// there is nothing to point at, and nothing is written.
func printAddressBanner(io *selfinstall.IO, address, note string) {
	if address == "" {
		return
	}
	io.Say("%s", addressBanner(address, note, outIsTerminal(io)))
}

// outIsTerminal reports whether this command's own output is a terminal, which
// is what decides the rule character: the encoding of a file, a pipe or a
// pasted report is not ours to assume.
func outIsTerminal(io *selfinstall.IO) bool {
	f, ok := io.Out.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
