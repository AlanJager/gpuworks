package main

import (
	"strings"
	"testing"
)

func TestSplitFrontmatterRequiresLeadingDelimiter(t *testing.T) {
	_, _, ok := splitFrontmatter("# no frontmatter here\n")
	if ok {
		t.Fatal("expected ok=false for a file with no frontmatter")
	}
}

func TestSplitFrontmatterRoundTrips(t *testing.T) {
	yamlText, body, ok := splitFrontmatter("---\ntitle: x\n---\n\n# body\n")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if yamlText != "title: x" {
		t.Fatalf("yamlText = %q", yamlText)
	}
	if body != "\n# body\n" {
		t.Fatalf("body = %q", body)
	}
}

func TestValidateDocValidPasses(t *testing.T) {
	d := Doc{RelPath: "artifacts/plan/plan-a.md", frontmatter: frontmatter{
		Title: "t", Status: "active", Created: "2026-09-01", Updated: "2026-09-02", Summary: "s",
	}}
	known := map[string]bool{d.RelPath: true}
	if errs := validateDoc(d, known); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateDocMissingFieldsFail(t *testing.T) {
	d := Doc{RelPath: "artifacts/plan/plan-a.md"}
	errs := validateDoc(d, map[string]bool{})
	if len(errs) == 0 {
		t.Fatal("expected errors for an entirely empty frontmatter")
	}
	joined := errsString(errs)
	for _, want := range []string{"title", "summary", "status", "created", "updated"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected an error mentioning %q, got: %s", want, joined)
		}
	}
}

func TestValidateDocInvalidStatusFails(t *testing.T) {
	d := Doc{RelPath: "p.md", frontmatter: frontmatter{
		Title: "t", Status: "in-progress", Created: "2026-09-01", Updated: "2026-09-02", Summary: "s",
	}}
	errs := validateDoc(d, map[string]bool{})
	if !anyContains(errs, "invalid status") {
		t.Fatalf("expected an invalid-status error, got %v", errs)
	}
}

func TestValidateDocBadDateFails(t *testing.T) {
	d := Doc{RelPath: "p.md", frontmatter: frontmatter{
		Title: "t", Status: "done", Created: "2026/09/01", Updated: "2026-09-02", Summary: "s",
	}}
	errs := validateDoc(d, map[string]bool{})
	if !anyContains(errs, "not a valid YYYY-MM-DD date") {
		t.Fatalf("expected a bad-date error, got %v", errs)
	}
}

func TestValidateDocSupersededWithoutTargetFails(t *testing.T) {
	d := Doc{RelPath: "p.md", frontmatter: frontmatter{
		Title: "t", Status: "superseded", Created: "2026-09-01", Updated: "2026-09-02", Summary: "s",
	}}
	errs := validateDoc(d, map[string]bool{})
	if !anyContains(errs, "superseded_by is empty") {
		t.Fatalf("expected a missing-superseded_by error, got %v", errs)
	}
}

func TestValidateDocSupersededByUnknownTargetFails(t *testing.T) {
	d := Doc{RelPath: "p.md", frontmatter: frontmatter{
		Title: "t", Status: "superseded", Created: "2026-09-01", Updated: "2026-09-02", Summary: "s",
		SupersededBy: "artifacts/plan/does-not-exist.md",
	}}
	errs := validateDoc(d, map[string]bool{"p.md": true})
	if !anyContains(errs, "does not exist or is out of scope") {
		t.Fatalf("expected a dangling superseded_by error, got %v", errs)
	}
}

func TestValidateDocDroppedWithoutOutcomeFails(t *testing.T) {
	d := Doc{RelPath: "p.md", frontmatter: frontmatter{
		Title: "t", Status: "dropped", Created: "2026-09-01", Updated: "2026-09-02", Summary: "s",
	}}
	errs := validateDoc(d, map[string]bool{})
	if !anyContains(errs, "outcome is empty") {
		t.Fatalf("expected a missing-outcome error for a dropped doc, got %v", errs)
	}
}

func TestValidateDocDroppedWithOutcomePasses(t *testing.T) {
	d := Doc{RelPath: "p.md", frontmatter: frontmatter{
		Title: "t", Status: "dropped", Created: "2026-09-01", Updated: "2026-09-02", Summary: "s",
		Outcome: "放弃的理由",
	}}
	if errs := validateDoc(d, map[string]bool{}); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateDocSupersededByOnNonSupersededFails(t *testing.T) {
	d := Doc{RelPath: "p.md", frontmatter: frontmatter{
		Title: "t", Status: "done", Created: "2026-09-01", Updated: "2026-09-02", Summary: "s",
		SupersededBy: "artifacts/plan/plan-b.md",
	}}
	errs := validateDoc(d, map[string]bool{"artifacts/plan/plan-b.md": true})
	if !anyContains(errs, "not superseded") {
		t.Fatalf("expected an error about superseded_by on a non-superseded doc, got %v", errs)
	}
}

func errsString(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		b.WriteString(e.Error())
		b.WriteString("\n")
	}
	return b.String()
}

func anyContains(errs []error, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), substr) {
			return true
		}
	}
	return false
}
