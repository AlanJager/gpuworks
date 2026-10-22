package main

import (
	"os"
	"path/filepath"
	"testing"
)

// newFakeRepo builds a minimal repository under t.TempDir(): an artifacts/plan.md
// carrying the generated-block markers, and placeholder
// artifacts/README.md / artifacts/.ignore so that -check (run before any
// -write) reports a clean "out of date" drift instead of a bare os.Open
// error. Tests then add topic directories under artifacts/work,
// artifacts/archive, or artifacts/research as needed.
func newFakeRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkfile(t, root, "artifacts/README.md", "placeholder\n")
	mkfile(t, root, "artifacts/.ignore", "placeholder\n")
	mkfile(t, root, planFile, "# plan\n\n"+genBeginMarker+"\n"+genEndMarker+"\n")
	return root
}

func mkfile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// supersededByPlaceholder is the token fm/fmWithOutcome put in
// superseded_by when status is "superseded"; tests that actually exercise
// the superseded path replace it with a real target path via
// strings.Replace, the same way they replace the fixed dates to make a doc
// look stale.
const supersededByPlaceholder = "SUPERSEDED_BY_PLACEHOLDER"

// fm builds a well-formed frontmatter block for a plan doc; tests mutate the
// returned string with strings.Replace to introduce one specific defect.
func fm(status string) string {
	s := "---\n" +
		"title: \"测试计划\"\n" +
		"status: " + status + "\n" +
		"created: 2026-09-01\n" +
		"updated: 2026-09-02\n" +
		"summary: \"一句话摘要\"\n"
	if status == "superseded" {
		s += "superseded_by: " + supersededByPlaceholder + "\n"
	}
	s += "---\n\n# 测试计划\n\n正文。\n"
	return s
}

// fmWithOutcome builds a well-formed frontmatter block like fm, but also
// sets an outcome field, used to test the outcome table column and
// dropped's required outcome.
func fmWithOutcome(status, outcome string) string {
	s := "---\n" +
		"title: \"测试计划\"\n" +
		"status: " + status + "\n" +
		"created: 2026-09-01\n" +
		"updated: 2026-09-02\n" +
		"summary: \"一句话摘要\"\n" +
		"outcome: \"" + outcome + "\"\n"
	if status == "superseded" {
		s += "superseded_by: " + supersededByPlaceholder + "\n"
	}
	s += "---\n\n# 测试计划\n\n正文。\n"
	return s
}
