package main

import (
	"testing"
)

func TestSelectEntryPrefersReadme(t *testing.T) {
	t1 := Topic{Dir: "artifacts/archive/2026-09-01-x", Docs: []Doc{
		{RelPath: "artifacts/archive/2026-09-01-x/README.md", frontmatter: frontmatter{Status: "done"}},
		{RelPath: "artifacts/archive/2026-09-01-x/plan.md", frontmatter: frontmatter{Status: "done"}},
	}}
	entry, ok := selectEntry(t1)
	if !ok {
		t.Fatal("expected an entry")
	}
	if entry.RelPath != "artifacts/archive/2026-09-01-x/README.md" {
		t.Fatalf("expected README.md to win over plan.md, got %s", entry.RelPath)
	}
}

func TestSelectEntryPrefersPlanMdOverOtherCandidates(t *testing.T) {
	t1 := Topic{Dir: "artifacts/archive/2026-09-01-x", Docs: []Doc{
		{RelPath: "artifacts/archive/2026-09-01-x/plan.md", frontmatter: frontmatter{Status: "done"}},
		{RelPath: "artifacts/archive/2026-09-01-x/plan-side.md", frontmatter: frontmatter{Status: "done"}},
	}}
	entry, ok := selectEntry(t1)
	if !ok {
		t.Fatal("expected an entry")
	}
	if entry.RelPath != "artifacts/archive/2026-09-01-x/plan.md" {
		t.Fatalf("expected plan.md to win, got %s", entry.RelPath)
	}
}

func TestSelectEntryPicksSingleNonTerminalCandidate(t *testing.T) {
	t1 := Topic{Dir: "artifacts/archive/2026-09-01-x", Docs: []Doc{
		{RelPath: "artifacts/archive/2026-09-01-x/plan-a.md", frontmatter: frontmatter{Status: "superseded", SupersededBy: "artifacts/archive/2026-09-01-x/plan-b.md"}},
		{RelPath: "artifacts/archive/2026-09-01-x/plan-b.md", frontmatter: frontmatter{Status: "done"}},
	}}
	entry, ok := selectEntry(t1)
	if !ok {
		t.Fatal("expected an entry")
	}
	if entry.RelPath != "artifacts/archive/2026-09-01-x/plan-b.md" {
		t.Fatalf("expected the single non-superseded doc to win, got %s", entry.RelPath)
	}
}

func TestSelectEntryAmbiguousWithTwoLiveCandidatesAndNoReadmeOrPlanMd(t *testing.T) {
	t1 := Topic{Dir: "artifacts/archive/2026-09-01-x", Docs: []Doc{
		{RelPath: "artifacts/archive/2026-09-01-x/plan-a.md", frontmatter: frontmatter{Status: "done"}},
		{RelPath: "artifacts/archive/2026-09-01-x/plan-b.md", frontmatter: frontmatter{Status: "done"}},
	}}
	if _, ok := selectEntry(t1); ok {
		t.Fatal("expected selectEntry to refuse to guess between two live candidates")
	}
}

func TestSelectEntryFailsWithZeroTopLevelDocs(t *testing.T) {
	t1 := Topic{Dir: "artifacts/archive/2026-09-01-x", Docs: nil}
	if _, ok := selectEntry(t1); ok {
		t.Fatal("expected selectEntry to fail with no candidates")
	}
}

func TestSelectEntryIgnoresNestedDocsForTopLevelSelection(t *testing.T) {
	// A nested doc (e.g. a round1-archive-style bundle) must not compete for
	// the topic's own entry, even if it would otherwise be the single live
	// candidate.
	t1 := Topic{Dir: "artifacts/archive/2026-09-01-x", Docs: []Doc{
		{RelPath: "artifacts/archive/2026-09-01-x/plan.md", frontmatter: frontmatter{Status: "done"}},
		{RelPath: "artifacts/archive/2026-09-01-x/nested/old-plan.md", frontmatter: frontmatter{Status: "done"}},
	}}
	entry, ok := selectEntry(t1)
	if !ok {
		t.Fatal("expected an entry")
	}
	if entry.RelPath != "artifacts/archive/2026-09-01-x/plan.md" {
		t.Fatalf("expected the top-level plan.md, got %s", entry.RelPath)
	}
}

func TestValidateTopicsFailsOnAmbiguousEntry(t *testing.T) {
	topics := []Topic{{Dir: "artifacts/archive/2026-09-01-x", Base: "artifacts/archive", Docs: []Doc{
		{RelPath: "artifacts/archive/2026-09-01-x/plan-a.md", frontmatter: frontmatter{Status: "done"}},
		{RelPath: "artifacts/archive/2026-09-01-x/plan-b.md", frontmatter: frontmatter{Status: "done"}},
	}}}
	errs := validateTopics(topics)
	if !anyContains(errs, "no unambiguous entry") {
		t.Fatalf("expected an ambiguous-entry error, got %v", errs)
	}
}

func TestValidateTopicsRequiresWorkEntryActiveOrProposed(t *testing.T) {
	topics := []Topic{{Dir: "artifacts/work/2026-09-01-x", Base: "artifacts/work", Docs: []Doc{
		{RelPath: "artifacts/work/2026-09-01-x/plan.md", frontmatter: frontmatter{Status: "done"}},
	}}}
	errs := validateTopics(topics)
	if !anyContains(errs, "work/ topics must have an active or proposed entry") {
		t.Fatalf("expected a work-entry-status error, got %v", errs)
	}
}

func TestValidateTopicsPassesWorkEntryProposed(t *testing.T) {
	topics := []Topic{{Dir: "artifacts/work/2026-09-01-x", Base: "artifacts/work", Docs: []Doc{
		{RelPath: "artifacts/work/2026-09-01-x/plan.md", frontmatter: frontmatter{Status: "proposed"}},
	}}}
	if errs := validateTopics(topics); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateTopicsRejectsActiveDocInArchive(t *testing.T) {
	topics := []Topic{{Dir: "artifacts/archive/2026-09-01-x", Base: "artifacts/archive", Docs: []Doc{
		{RelPath: "artifacts/archive/2026-09-01-x/plan.md", frontmatter: frontmatter{Status: "done"}},
		{RelPath: "artifacts/archive/2026-09-01-x/nested/side.md", frontmatter: frontmatter{Status: "active"}},
	}}}
	errs := validateTopics(topics)
	if !anyContains(errs, "must be closed (not active/proposed)") {
		t.Fatalf("expected an active-in-archive error, got %v", errs)
	}
}

func TestValidateTopicsRejectsProposedDocInResearch(t *testing.T) {
	topics := []Topic{{Dir: "artifacts/research/2026-09-01-x", Base: "artifacts/research", Docs: []Doc{
		{RelPath: "artifacts/research/2026-09-01-x/README.md", frontmatter: frontmatter{Status: "proposed"}},
	}}}
	errs := validateTopics(topics)
	if !anyContains(errs, "must be closed (not active/proposed)") {
		t.Fatalf("expected a proposed-in-research error, got %v", errs)
	}
}

func TestValidateTopicsRejectsBadDirectoryName(t *testing.T) {
	topics := []Topic{{Dir: "artifacts/archive/images", Base: "artifacts/archive", Docs: []Doc{
		{RelPath: "artifacts/archive/images/plan.md", frontmatter: frontmatter{Status: "done"}},
	}}}
	errs := validateTopics(topics)
	if !anyContains(errs, "YYYY-MM-DD-<slug>") {
		t.Fatalf("expected a bad-directory-name error, got %v", errs)
	}
}

func TestTopicsWithEntriesDropsAmbiguousTopics(t *testing.T) {
	topics := []Topic{
		{Dir: "artifacts/archive/2026-09-01-ambiguous", Base: "artifacts/archive", Docs: []Doc{
			{RelPath: "artifacts/archive/2026-09-01-ambiguous/plan-a.md", frontmatter: frontmatter{Status: "done"}},
			{RelPath: "artifacts/archive/2026-09-01-ambiguous/plan-b.md", frontmatter: frontmatter{Status: "done"}},
		}},
		{Dir: "artifacts/archive/2026-09-02-clean", Base: "artifacts/archive", Docs: []Doc{
			{RelPath: "artifacts/archive/2026-09-02-clean/plan.md", frontmatter: frontmatter{Status: "done"}},
		}},
	}
	out := topicsWithEntries(topics)
	if len(out) != 1 || out[0].Dir != "artifacts/archive/2026-09-02-clean" {
		t.Fatalf("expected only the unambiguous topic, got %v", out)
	}
}

func TestIsTopicDir(t *testing.T) {
	cases := map[string]bool{
		"artifacts/work/2026-09-01-x":           true,
		"artifacts/archive/2026-09-01-x":        true,
		"artifacts/research/2026-09-01-x":       true,
		"artifacts/archive/todo-resolved":       false,
		"artifacts/archive/2026-09-01-x/nested": false,
		"artifacts/work":                        false,
		"artifacts":                             false,
		"docs/adr":                              false,
	}
	for dir, want := range cases {
		if got := isTopicDir(dir); got != want {
			t.Errorf("isTopicDir(%q) = %v, want %v", dir, got, want)
		}
	}
}
