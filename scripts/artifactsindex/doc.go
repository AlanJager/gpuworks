// Package main implements the artifactsindex tool: it reads the YAML
// frontmatter that every topic's entry document under artifacts/{work,
// archive,research} (see scope.go and topics.go) carries as its single
// source of truth for status/dates/summary, and either regenerates the
// derived indexes (-write: artifacts/README.md, artifacts/.ignore, and the
// generated block in plan.md) or verifies the repository already matches
// what would be generated (-check, the default).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// validStatuses is the closed enum a plan document's frontmatter status must
// belong to. Order here also fixes the group order used when rendering the
// generated tables (active, proposed, done, superseded, dropped).
var validStatuses = []string{"active", "proposed", "done", "superseded", "dropped"}

func isValidStatus(s string) bool {
	for _, v := range validStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// frontmatter mirrors the fixed YAML schema documented in CLAUDE.md: title,
// status, created, updated, summary, an optional one-line outcome (required
// when status is dropped), and (only for status: superseded) a
// superseded_by pointer. All fields decode as plain strings; dates are kept
// as strings (YYYY-MM-DD) and parsed on demand so a malformed date is a
// validation error rather than a silent zero time.
type frontmatter struct {
	Title        string `yaml:"title"`
	Status       string `yaml:"status"`
	Created      string `yaml:"created"`
	Updated      string `yaml:"updated"`
	Summary      string `yaml:"summary"`
	Outcome      string `yaml:"outcome"`
	SupersededBy string `yaml:"superseded_by"`
}

// Doc is a validated plan document: its scope-relative path (forward
// slashes, relative to the repo root) plus its decoded frontmatter.
type Doc struct {
	RelPath string
	frontmatter
}

const fmDelim = "---"

// splitFrontmatter separates a leading "---\n...\n---\n" YAML block from the
// rest of the file. It returns the raw YAML text (without the delimiters)
// and the remaining body (untouched, including its own leading blank line).
// ok is false when the file has no frontmatter block at all.
func splitFrontmatter(content string) (yamlText string, body string, ok bool) {
	if !strings.HasPrefix(content, fmDelim+"\n") {
		return "", content, false
	}
	rest := content[len(fmDelim)+1:]
	// The closing delimiter must be its own line: "\n---\n" or "\n---" at EOF.
	idx := strings.Index(rest, "\n"+fmDelim)
	if idx < 0 {
		return "", content, false
	}
	yamlText = rest[:idx]
	afterClose := rest[idx+len("\n"+fmDelim):]
	afterClose = strings.TrimPrefix(afterClose, "\n")
	return yamlText, afterClose, true
}

// parseDoc reads path (absolute) and returns a Doc whose RelPath is relPath.
// It only parses; validation (required fields, enum, dates, superseded_by
// target) is validateDoc's job so callers can collect every problem instead
// of stopping at the first file.
//
// The scope this package scans (scopeFiles) is now every .md file under
// artifacts/{work,archive,research}, most of which are plain evidence
// (acceptance records, reports) that never carry frontmatter at all. A file
// that does not even start with the "---\n" opening delimiter is therefore
// not an error: isDoc is false and err is nil, meaning "not a plan
// document, skip it". A file that starts the delimiter but is malformed
// (no closing "---" line, or invalid YAML inside) is a real error: the
// author clearly intended frontmatter and got it wrong.
func parseDoc(root, relPath string) (doc Doc, isDoc bool, err error) {
	full := filepath.Join(root, relPath)
	raw, err := os.ReadFile(full)
	if err != nil {
		return Doc{}, false, fmt.Errorf("%s: %w", relPath, err)
	}
	if !strings.HasPrefix(string(raw), fmDelim+"\n") {
		return Doc{}, false, nil
	}
	yamlText, _, ok := splitFrontmatter(string(raw))
	if !ok {
		return Doc{}, false, fmt.Errorf("%s: starts with \"---\" but frontmatter block is never closed with a line containing only \"---\"", relPath)
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(yamlText), &fm); err != nil {
		return Doc{}, false, fmt.Errorf("%s: invalid frontmatter YAML: %w", relPath, err)
	}
	return Doc{RelPath: filepath.ToSlash(relPath), frontmatter: fm}, true, nil
}

// validateDoc checks the required-field, enum, date-format, and
// superseded_by-target rules from CLAUDE.md. known is the full set of
// scope-relative paths (used to check that a superseded_by target exists and
// is itself in scope, since a plan can only be superseded by another plan).
func validateDoc(d Doc, known map[string]bool) []error {
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{d.RelPath}, a...)...))
	}

	if strings.TrimSpace(d.Title) == "" {
		fail("missing required field: title")
	}
	if strings.TrimSpace(d.Summary) == "" {
		fail("missing required field: summary")
	}
	if strings.TrimSpace(d.Status) == "" {
		fail("missing required field: status")
	} else if !isValidStatus(d.Status) {
		fail("invalid status %q; must be one of %v", d.Status, validStatuses)
	}
	for _, pair := range []struct{ name, val string }{{"created", d.Created}, {"updated", d.Updated}} {
		if strings.TrimSpace(pair.val) == "" {
			fail("missing required field: %s", pair.name)
			continue
		}
		if _, err := time.Parse("2006-01-02", pair.val); err != nil {
			fail("field %s is not a valid YYYY-MM-DD date: %q", pair.name, pair.val)
		}
	}
	if d.Status == "superseded" {
		if strings.TrimSpace(d.SupersededBy) == "" {
			fail("status is superseded but superseded_by is empty")
		} else if !known[d.SupersededBy] {
			fail("superseded_by target does not exist or is out of scope: %s", d.SupersededBy)
		}
	} else if strings.TrimSpace(d.SupersededBy) != "" {
		fail("superseded_by is set but status is %q, not superseded", d.Status)
	}
	if d.Status == "dropped" && strings.TrimSpace(d.Outcome) == "" {
		fail("status is dropped but outcome is empty")
	}
	return errs
}
