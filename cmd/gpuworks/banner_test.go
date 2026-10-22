package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

func TestPickBannerChoosesByTerminalAndLocale(t *testing.T) {
	cases := []struct {
		name     string
		terminal bool
		utf8     bool
		want     string
	}{
		{"utf8 terminal", true, true, bannerShadow},
		{"utf8 locale but redirected", false, true, bannerASCII},
		{"terminal under a C locale", true, false, bannerASCII},
		{"neither", false, false, bannerASCII},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickBanner(tc.terminal, tc.utf8); got != tc.want {
				t.Fatalf("pickBanner(%v, %v) returned the other art", tc.terminal, tc.utf8)
			}
		})
	}
}

func TestLocaleIsUTF8(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"LANG", map[string]string{"LANG": "en_US.UTF-8"}, true},
		{"LANG in lower case", map[string]string{"LANG": "zh_CN.utf8"}, true},
		{"LC_CTYPE alone", map[string]string{"LC_CTYPE": "C.UTF-8"}, true},
		{"LC_ALL wins over LANG", map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, false},
		{"empty LC_ALL falls through", map[string]string{"LC_ALL": "", "LANG": "en_US.UTF-8"}, true},
		{"empty LC_ALL falls through to LC_CTYPE", map[string]string{"LC_ALL": "", "LC_CTYPE": "C", "LANG": "en_US.UTF-8"}, false},
		{"C locale", map[string]string{"LANG": "C"}, false},
		{"nothing set", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Every locale variable is pinned, so the ambient environment
			// cannot decide the answer for us.
			for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
				t.Setenv(key, tc.env[key])
			}
			if got := localeIsUTF8(); got != tc.want {
				t.Fatalf("localeIsUTF8() = %v, want %v (env %v)", got, tc.want, tc.env)
			}
		})
	}
}

// The art is data, so the invariants it has to keep are worth asserting: the
// hand-drawn glyphs have to stay aligned, and neither banner may wrap an
// ordinary 80-column terminal.
func TestBannerArtIsWellFormed(t *testing.T) {
	cases := []struct {
		name  string
		art   string
		lines int
	}{
		{"ascii", bannerASCII, 5},
		{"shadow", bannerShadow, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := strings.Split(tc.art, "\n")
			if len(lines) != tc.lines {
				t.Fatalf("%d lines, want %d", len(lines), tc.lines)
			}
			for i, line := range lines {
				if strings.TrimSpace(line) == "" {
					t.Fatalf("line %d is blank", i+1)
				}
				if strings.TrimRight(line, " \t") != line {
					t.Errorf("line %d has trailing whitespace: %q", i+1, line)
				}
				if width := len([]rune(line)); width > 80 {
					t.Errorf("line %d is %d columns wide, too wide for an 80-column terminal", i+1, width)
				}
			}
		})
	}
}

func TestASCIIBannerStaysASCIIAndShadowDoesNot(t *testing.T) {
	for i, b := range []byte(bannerASCII) {
		if b >= 0x80 {
			t.Fatalf("the ASCII banner has a non-ASCII byte at %d (%q); it is the one that has to survive C locales and byte-mangling logs", i, b)
		}
	}
	shadowLines := strings.Split(bannerShadow, "\n")
	for i, line := range shadowLines {
		if len([]rune(line)) == len(line) {
			t.Fatalf("shadow line %d has no block glyphs: %q", i+1, line)
		}
	}
}

func TestStdoutIsTerminalRejectsEverythingButATerminal(t *testing.T) {
	var buf bytes.Buffer
	if stdoutIsTerminal(&buf) {
		t.Error("a buffer reported itself as a terminal")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if stdoutIsTerminal(r) || stdoutIsTerminal(w) {
		t.Error("a pipe reported itself as a terminal")
	}
}

// The banner opens an interactive install, padded with two blank lines on
// each side; --quiet drops the whole block, because that is what makes an
// install scriptable.
func TestPrintInstallIntroShowsTheBannerUnlessQuiet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quiet bool
		want  string
	}{
		// Say appends the line terminator, so the art's last line plus two
		// blank lines are three newlines in total.
		{"interactive", false, "\n\n" + bannerASCII + "\n\n\n"},
		{"quiet", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			io := &selfinstall.IO{Out: &out, Err: &errOut}
			printInstallIntro(io, tc.quiet)

			if printed := out.String(); printed != tc.want {
				t.Fatalf("intro = %q, want %q", printed, tc.want)
			}
		})
	}
}

// The banner belongs to the opening, not to the summary: a report must never
// print it, and must keep the summary that carries a fresh installation's
// password.
func TestPrintInstallReportHasTheSummaryAndNoBanner(t *testing.T) {
	report := selfinstall.InstallReport{
		Layout:      selfinstall.NewLayout("/opt/gpuworks", "", ""),
		Mode:        selfinstall.ModeSystemd,
		ServiceUser: "gpuworks",
		AdminUser:   "admin",
		Password:    "generated-password",
		URL:         "http://10.0.0.5:8800",
		Credentials: "/opt/gpuworks/data/initial-credentials",
		ReleaseID:   "abc1234",
	}
	bannerFirstLine := strings.SplitN(bannerASCII, "\n", 2)[0]

	var out, errOut bytes.Buffer
	io := &selfinstall.IO{Out: &out, Err: &errOut}
	printInstallReport(io, report)

	printed := out.String()
	if strings.Contains(printed, bannerFirstLine) {
		t.Fatalf("the report printed the banner:\n%s", printed)
	}
	for _, want := range []string{"gpuworks abc1234 installed", "generated-password", "initial-credentials"} {
		if !strings.Contains(printed, want) {
			t.Errorf("the summary is missing %q:\n%s", want, printed)
		}
	}
}
