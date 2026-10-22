package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Topic is one work/*, archive/* (excluding todoResolvedDir), or research/*
// directory: a unit of work that groups a set of plan documents (and,
// alongside them on disk but outside this package's concern, arbitrary
// evidence files) under one YYYY-MM-DD-<topic> directory name.
type Topic struct {
	// Dir is the topic directory, repo-root-relative, forward-slashed, e.g.
	// "artifacts/archive/2026-08-20-images".
	Dir string
	// Base is which of the three buckets Dir lives under: "artifacts/work",
	// "artifacts/archive", or "artifacts/research".
	Base string
	// Docs is every frontmatter document found anywhere under Dir (any
	// depth), sorted by RelPath.
	Docs []Doc
	// Entry is the single document that represents the topic as a whole in
	// the generated indexes (artifacts/README.md, plan.md's generated
	// block). See selectEntry for how it is chosen.
	Entry Doc
}

// dateTopicName matches "<YYYY-MM-DD>-<slug>", the fixed naming convention
// for every topic directory (see CLAUDE.md).
var dateTopicNameOK = func(name string) bool {
	if len(name) < len("2026-01-01-x") {
		return false
	}
	for i, want := range "0000-00-00-" {
		if want == '0' {
			if name[i] < '0' || name[i] > '9' {
				return false
			}
			continue
		}
		if name[i] != byte(want) {
			return false
		}
	}
	return true
}

// buildTopics groups docs (every parsed frontmatter document under scope)
// into Topics, one per topic directory that actually exists on disk (found
// via allMdFiles's directories, so an empty topic directory with zero
// frontmatter files still surfaces as a Topic with Docs == nil, which
// validateTopic then reports as missing an entry).
func buildTopics(root string, allMdFiles []string, docs []Doc) ([]Topic, error) {
	docsByDir := map[string][]Doc{}
	for _, d := range docs {
		dir := filepath.ToSlash(filepath.Dir(d.RelPath))
		docsByDir[dir] = append(docsByDir[dir], d)
	}

	topicDirSet := map[string]bool{}
	for _, f := range allMdFiles {
		dir := filepath.ToSlash(filepath.Dir(f))
		for dir != "." && dir != "/" {
			if isTopicDir(dir) {
				topicDirSet[dir] = true
				break
			}
			parent := filepath.ToSlash(filepath.Dir(dir))
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	var topicDirs []string
	for d := range topicDirSet {
		topicDirs = append(topicDirs, d)
	}
	sort.Strings(topicDirs)

	var topics []Topic
	for _, dir := range topicDirs {
		base, _ := topicBase(dir)
		var topicDocs []Doc
		for sub, ds := range docsByDir {
			if sub == dir || strings.HasPrefix(sub, dir+"/") {
				topicDocs = append(topicDocs, ds...)
			}
		}
		sort.Slice(topicDocs, func(i, j int) bool { return topicDocs[i].RelPath < topicDocs[j].RelPath })
		topics = append(topics, Topic{Dir: dir, Base: base, Docs: topicDocs})
	}
	return topics, nil
}

// selectEntry picks the one document (among a topic's top-level docs, i.e.
// those directly inside t.Dir, not a nested evidence subdirectory) that
// represents the topic. In order:
//  1. <dir>/README.md, if it carries frontmatter.
//  2. <dir>/plan.md, if it carries frontmatter.
//  3. The single top-level doc whose status is not superseded/dropped, if
//     there is exactly one such doc.
//
// Any other shape (zero top-level docs, or more than one non-terminal
// candidate with neither README.md nor plan.md to disambiguate) is
// ambiguous: ok is false and the caller must report it rather than guess.
func selectEntry(t Topic) (Doc, bool) {
	var topLevel []Doc
	for _, d := range t.Docs {
		if filepath.ToSlash(filepath.Dir(d.RelPath)) == t.Dir {
			topLevel = append(topLevel, d)
		}
	}
	for _, d := range topLevel {
		if filepath.Base(d.RelPath) == "README.md" {
			return d, true
		}
	}
	for _, d := range topLevel {
		if filepath.Base(d.RelPath) == "plan.md" {
			return d, true
		}
	}
	var live []Doc
	for _, d := range topLevel {
		if d.Status != "superseded" && d.Status != "dropped" {
			live = append(live, d)
		}
	}
	if len(live) == 1 {
		return live[0], true
	}
	return Doc{}, false
}

// validateTopics checks the structural rules from CLAUDE.md that apply to
// the topic level rather than to one document: every topic has exactly one
// entry, work/ topics have a proposed/active entry, and archive/research
// topics (and every document inside them, not just the entry) are never
// active/proposed.
func validateTopics(topics []Topic) []error {
	var errs []error
	for _, t := range topics {
		if !dateTopicNameOK(filepath.Base(t.Dir)) {
			errs = append(errs, fmt.Errorf("%s: topic directory name must match YYYY-MM-DD-<slug>", t.Dir))
		}
		entry, ok := selectEntry(t)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: no unambiguous entry file (need a README.md, a plan.md, or exactly one top-level non-superseded/dropped document)", t.Dir))
			continue
		}
		t.Entry = entry
		switch t.Base {
		case "artifacts/work":
			if entry.Status != "active" && entry.Status != "proposed" {
				errs = append(errs, fmt.Errorf("%s: entry %s has status %q; work/ topics must have an active or proposed entry", t.Dir, entry.RelPath, entry.Status))
			}
		case "artifacts/archive", "artifacts/research":
			for _, d := range t.Docs {
				if d.Status == "active" || d.Status == "proposed" {
					errs = append(errs, fmt.Errorf("%s: %s has status %q; archive/research documents must be closed (not active/proposed) — move it back to artifacts/work/ instead", t.Dir, d.RelPath, d.Status))
				}
			}
		}
	}
	return errs
}

// topicsWithEntries returns topics with Entry populated, dropping any topic
// selectEntry could not resolve (validateTopics will already have reported
// those as errors; renderers should only run once validation passed).
func topicsWithEntries(topics []Topic) []Topic {
	out := make([]Topic, 0, len(topics))
	for _, t := range topics {
		if entry, ok := selectEntry(t); ok {
			t.Entry = entry
			out = append(out, t)
		}
	}
	return out
}
