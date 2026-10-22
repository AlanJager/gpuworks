// Command doclinks checks that relative Markdown links in the user-facing
// docs (docs/**, README.md, CONTEXT.md, CLAUDE.md) resolve to real files and,
// when they carry a #fragment, to a real heading in the target file. Under
// docs/ it also rejects [[wiki]] style cross references, which neither GitHub
// nor the Hugo site renders as links. CONTEXT.md is exempt: it is the domain
// glossary and uses [[term]] as its own cross-reference convention.
//
// Links into artifacts/ are not checked: it is a separate private repository
// that may be absent and is versioned on its own; scripts/artifactsindex
// reports stale references into it when it is checked out.
//
// Usage:
//
//	go run ./scripts/doclinks [-repo .]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	linkRe    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	wikiRe    = regexp.MustCompile(`\[\[[^\]]+\]\]`)
	headingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)
	fenceRe   = regexp.MustCompile("(?s)```.*?```")
	inlineRe  = regexp.MustCompile("`[^`\n]*`")
)

func main() {
	repo := flag.String("repo", ".", "repository root")
	flag.Parse()
	problems, err := check(*repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "doclinks:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(os.Stderr, "doclinks: %d problem(s)\n", len(problems))
		os.Exit(1)
	}
}

func scope(repo string) ([]string, error) {
	files := []string{}
	for _, f := range []string{"README.md", "CONTEXT.md", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err == nil {
			files = append(files, f)
		}
	}
	err := filepath.WalkDir(filepath.Join(repo, "docs"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			rel, _ := filepath.Rel(repo, p)
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

// stripCode blanks fenced and inline code so that code samples containing
// "](" or "[[" are not mistaken for links.
func stripCode(s string) string {
	s = fenceRe.ReplaceAllString(s, "")
	return inlineRe.ReplaceAllString(s, "")
}

// slug follows GitHub's heading anchor rules closely enough for CJK headings:
// lower-case, keep letters/digits (CJK included), drop punctuation and symbols
// (full-width ones too) except '-' and '_', spaces become '-'.
func slug(h string) string {
	h = inlineRe.ReplaceAllStringFunc(h, func(s string) string { return strings.Trim(s, "`") })
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(h)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || r == '_':
			b.WriteRune(r)
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func anchors(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range headingRe.FindAllStringSubmatch(fenceRe.ReplaceAllString(string(raw), ""), -1) {
		out[slug(m[1])] = true
	}
	return out, nil
}

func check(repo string) ([]string, error) {
	files, err := scope(repo)
	if err != nil {
		return nil, err
	}
	var problems []string
	cache := map[string]map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(repo, f))
		if err != nil {
			return nil, err
		}
		body := stripCode(string(raw))
		if strings.HasPrefix(f, "docs"+string(filepath.Separator)) {
			for _, w := range wikiRe.FindAllString(body, -1) {
				problems = append(problems, fmt.Sprintf("%s: wiki-style reference %s (use a normal Markdown link)", f, w))
			}
		}
		for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			pathPart, frag, _ := strings.Cut(target, "#")
			resolved := filepath.Join(repo, filepath.Dir(f), pathPart)
			if pathPart == "" {
				resolved = filepath.Join(repo, f)
			}
			if rel, err := filepath.Rel(repo, resolved); err == nil && (rel == "artifacts" || strings.HasPrefix(rel, "artifacts"+string(filepath.Separator))) {
				continue
			}
			st, err := os.Stat(resolved)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s: broken link %s", f, target))
				continue
			}
			if frag == "" || st.IsDir() || !strings.HasSuffix(resolved, ".md") {
				continue
			}
			set, ok := cache[resolved]
			if !ok {
				if set, err = anchors(resolved); err != nil {
					return nil, err
				}
				cache[resolved] = set
			}
			if !set[frag] {
				problems = append(problems, fmt.Sprintf("%s: missing anchor %s", f, target))
			}
		}
	}
	return problems, nil
}
