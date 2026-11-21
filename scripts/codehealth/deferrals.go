package main

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// budgetsFile holds the architecture ratchet (scripts/arch-constraints.md).
const budgetsFile = "scripts/arch-constraints.txt"

type budget struct {
	value int
	kind  string
}

func budgetsAt(commit string) (map[string]budget, error) {
	out, err := git("show", commit+":"+budgetsFile)
	if err != nil {
		return nil, err
	}
	return parseBudgets(out), nil
}

func parseBudgets(text string) map[string]budget {
	m := map[string]budget{}
	for _, line := range strings.Split(text, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if v, err := strconv.Atoi(f[1]); err == nil {
			m[f[0]] = budget{v, f[2]}
		}
	}
	return m
}

// loosened says whether a budget moved the way the ratchet forbids: a max
// (or warn) upward, a min downward.
func loosened(before, after budget) bool {
	if after.kind == "min" {
		return after.value < before.value
	}
	return after.value > before.value
}

// added is one line a diff added to a production source file.
type added struct {
	file string
	line int
	text string
}

// sourcePaths limit the diff to code the product ships.
var sourcePaths = []string{"cmd", "internal", "pkg", "ui/src",
	":!*_test.go", ":!*_gen.go", ":!*.test.ts", ":!*.test.tsx", ":!*/testdata/*",
	":!ui/src/i18n", ":!*.json", ":!*.md", ":!*.yaml", ":!*.yml"}

func addedLines(base, head string) ([]added, error) {
	out, err := git(append([]string{"diff", "-U0", "--no-renames", base, head, "--"}, sourcePaths...)...)
	if err != nil {
		return nil, err
	}
	return parseAdded(out), nil
}

var hunk = regexp.MustCompile(`^@@ -\S+ \+(\d+)`)

func parseAdded(diff string) []added {
	var out []added
	var file string
	var line int
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		t := sc.Text()
		switch {
		case strings.HasPrefix(t, "+++ "):
			file = strings.TrimPrefix(strings.TrimPrefix(t, "+++ "), "b/")
		case strings.HasPrefix(t, "@@"):
			if m := hunk.FindStringSubmatch(t); m != nil {
				line, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(t, "+"):
			out = append(out, added{file, line, t[1:]})
			line++
		}
	}
	return out
}

// marker is one kind of added line that puts work off.
type marker struct {
	name string
	re   *regexp.Regexp
}

// comment matches the comment part of a line, in Go and TypeScript.
const comment = `(?://|/\*|\{/\*).*`

var markers = []marker{
	{"lint suppression", regexp.MustCompile(`//\s*nolint|eslint-disable|@ts-ignore|@ts-expect-error`)},
	{"todo marker", regexp.MustCompile(comment + `\b(?:TODO|FIXME|HACK|XXX)\b`)},
	{"put off in words", regexp.MustCompile(`(?i)` + comment + `(?:\bfor now\b|\bworkaround\b|\bstop-?gap\b|change of its own|separate (?:change|commit)|not yet run|\bnot yet (?:implemented|supported|wired|enforced)|暂时|临时|后续再|待重构)`)},
	{"older-version compatibility", regexp.MustCompile(`(?i)` + comment + `(?:\blegacy\b|older agent|predates|backward.compat)`)},
	{"copy kept in step by hand", regexp.MustCompile(`(?i)` + comment + `(?:\bmirrors the\b|keep in sync|kept in step|cannot import)`)},
}

func reportDeferrals(w io.Writer, base, head string, top int) error {
	fmt.Fprintf(w, "## Deferrals\n\n")
	before, err := budgetsAt(base)
	if err != nil {
		return err
	}
	after, err := budgetsAt(head)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "### Architecture budgets loosened (%s)\n\n| budget | base | head |\n|---|---|---|\n", budgetsFile)
	n, fresh := 0, 0
	for _, kv := range sortedCounts(func() map[string]int {
		m := map[string]int{}
		for k, a := range after {
			b, ok := before[k]
			if !ok {
				fresh++
				continue
			}
			if loosened(b, a) {
				m[k] = a.value - b.value
			}
		}
		return m
	}()) {
		fmt.Fprintf(w, "| %s | %d | %d |\n", kv.k, before[kv.k].value, after[kv.k].value)
		n++
	}
	fmt.Fprintf(w, "\n%d loosened, %d new budgets.\n\n", n, fresh)

	lines, err := addedLines(base, head)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "### Added lines that put work off (%d production lines added)\n\n", len(lines))
	for _, m := range markers {
		var hits []added
		for _, l := range lines {
			if m.re.MatchString(l.text) {
				hits = append(hits, l)
			}
		}
		fmt.Fprintf(w, "**%s: %d**\n\n", m.name, len(hits))
		for i, h := range hits {
			if i == top {
				fmt.Fprintf(w, "- … %d more\n", len(hits)-top)
				break
			}
			text := strings.TrimSpace(h.text)
			if len(text) > 200 {
				text = text[:200] + "…"
			}
			fmt.Fprintf(w, "- `%s:%d` %s\n", h.file, h.line, strings.ReplaceAll(text, "`", "'"))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "### Build-tagged production files\n\n| | base | head |\n|---|---|---|\n")
	tb, err := taggedFiles(base)
	if err != nil {
		return err
	}
	th, err := taggedFiles(head)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "| files with a //go:build line | %d | %d |\n\n", tb, th)
	return nil
}

func taggedFiles(commit string) (int, error) {
	n := 0
	err := eachGoFile(commit, func(_ string, src []byte) {
		for _, line := range strings.SplitN(string(src), "\n", 40) {
			if strings.HasPrefix(line, "//go:build ") {
				n++
				return
			}
			if strings.HasPrefix(line, "package ") {
				return
			}
		}
	})
	return n, err
}
