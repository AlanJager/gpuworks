package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// The address is the thing an operator copies, so its framing is worth
// asserting: the rules, the wrapping, and — most of all — that a redirected
// stream does not get the box-drawing characters.

func TestAddressBannerFramesTheAddress(t *testing.T) {
	const url = "https://tall-stewart-suggests-supervisor.trycloudflare.com"
	got := addressBanner(url, "temporary address: it changes when the connector restarts", true)
	lines := strings.Split(got, "\n")

	if len(lines) < 4 {
		t.Fatalf("banner has %d lines, want rules, heading, address and a note:\n%s", len(lines), got)
	}
	if lines[0] != strings.Repeat("═", bannerWidth) || lines[len(lines)-1] != strings.Repeat("═", bannerWidth) {
		t.Errorf("the rules should frame the banner at %d columns:\n%s", bannerWidth, got)
	}
	if !strings.Contains(lines[1], "PUBLIC ADDRESS") {
		t.Errorf("the heading should name what the framed value is: %q", lines[1])
	}
	// The address is on its own line, unbroken: it is the thing being copied.
	if strings.TrimSpace(lines[2]) != url {
		t.Errorf("address line = %q, want the address alone", lines[2])
	}
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > bannerWidth {
			t.Errorf("line is %d columns wide, want at most %d: %q", n, bannerWidth, line)
		}
	}
}

// A redirected stream gets "=": the frame is for a person reading a terminal,
// and a log file or a pasted report should not carry box-drawing characters.
func TestAddressBannerIsAsciiWhenRedirected(t *testing.T) {
	got := addressBanner("https://x.trycloudflare.com", "note", false)
	if strings.ContainsAny(got, "═║") {
		t.Errorf("a redirected stream must not get box-drawing characters:\n%s", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("=", bannerWidth)) {
		t.Errorf("the plain rule should still frame the address:\n%s", got)
	}
}

// The note is a sentence. It wraps inside the frame instead of widening it past
// the terminal.
func TestAddressBannerWrapsALongNote(t *testing.T) {
	note := strings.Repeat("word ", 40)
	got := addressBanner("https://gpu.example.com", note, false)
	for _, line := range strings.Split(got, "\n") {
		if n := utf8.RuneCountInString(line); n > bannerWidth {
			t.Fatalf("line is %d columns wide, want at most %d: %q", n, bannerWidth, line)
		}
	}
	if strings.Count(got, "word") != 40 {
		t.Errorf("wrapping must not lose words:\n%s", got)
	}
}

// An address too long for the frame widens it rather than being cut.
func TestAddressBannerWidensRatherThanCutting(t *testing.T) {
	long := "https://" + strings.Repeat("a", 90) + ".trycloudflare.com"
	got := addressBanner(long, "", false)
	if !strings.Contains(got, long) {
		t.Fatalf("the address must survive intact:\n%s", got)
	}
}

// Without an address there is nothing to point at.
func TestPrintAddressBannerWithoutAnAddress(t *testing.T) {
	out := &bytes.Buffer{}
	io := &selfinstall.IO{In: strings.NewReader(""), Out: out, Err: out}
	printAddressBanner(io, "", "temporary")
	if out.Len() != 0 {
		t.Fatalf("an empty address must print nothing, got %q", out.String())
	}
	printAddressBanner(io, "https://gpu.example.com", "")
	if !strings.Contains(out.String(), "https://gpu.example.com") {
		t.Fatalf("a real address must be printed, got %q", out.String())
	}
}
