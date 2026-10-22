package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scopeBases are the three top-level buckets that make up the artifacts/
// three-way split (see CLAUDE.md and artifacts/README.md): work/ (in
// progress), archive/ (closed), research/ (independent, done). Every .md
// file anywhere under these three directories is a scope candidate; whether
// it actually carries frontmatter (and is therefore an indexed "plan
// document") is decided per-file by parseDoc, not by path shape, because
// most files under archive/ and research/ are plain evidence (acceptance
// records, reports, screenshots-with-README) that never had and never will
// have frontmatter.
var scopeBases = []string{"artifacts/work", "artifacts/archive", "artifacts/research"}

// todoResolvedDir is the one exception carved out of artifacts/archive/: a
// flat, month-bucketed log of resolved TODOs (artifacts/archive/todo-resolved/
// <YYYY-MM>.md), not a topic directory, and its files never carry
// frontmatter. scopeFiles still walks it (a stray frontmatter file there
// would still be found and would still error out in parseDoc, which is the
// right failure mode), but topic discovery (topics.go) must not treat it as
// a topic requiring an entry file.
const todoResolvedDir = "artifacts/archive/todo-resolved"

// scopeFiles returns every .md file under the three scope bases, as a path
// relative to root, forward-slashed, sorted for determinism. It does not
// filter by frontmatter presence; that is parseDoc's job so a single walk
// serves both document discovery and (in isCodeOrConfig's docs-tree sibling
// in links.go) nothing else needs its own separate walk.
func scopeFiles(root string) ([]string, error) {
	var out []string
	for _, base := range scopeBases {
		baseAbs := filepath.Join(root, base)
		if _, err := os.Stat(baseAbs); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(baseAbs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// isTopicDir reports whether relDir (repo-root-relative, forward-slashed,
// e.g. "artifacts/archive/2026-08-20-images") is a topic directory: an
// immediate child of one of the three scope bases, excluding
// todoResolvedDir.
func isTopicDir(relDir string) bool {
	if relDir == todoResolvedDir {
		return false
	}
	for _, base := range scopeBases {
		parent := base + "/"
		if strings.HasPrefix(relDir, parent) && !strings.Contains(relDir[len(parent):], "/") {
			return true
		}
	}
	return false
}

// topicBase returns which of the three scope bases relDir's topic lives
// under ("artifacts/work", "artifacts/archive", or "artifacts/research"),
// and ok=false if relDir is not a topic directory at all.
func topicBase(relDir string) (base string, ok bool) {
	if !isTopicDir(relDir) {
		return "", false
	}
	for _, b := range scopeBases {
		if strings.HasPrefix(relDir, b+"/") {
			return b, true
		}
	}
	return "", false
}
