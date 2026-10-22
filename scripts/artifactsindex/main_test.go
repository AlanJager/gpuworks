package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustNow(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestCheckFailsOnUnwrittenRepo(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed"))
	mkfile(t, root, "artifacts/archive/2026-09-02-b/plan.md", fm("done"))

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected check to fail before -write has ever generated the indexes")
	}
	if !strings.Contains(out.String(), "out of date") {
		t.Fatalf("expected an out-of-date message, got: %s", out.String())
	}
}

func TestWriteThenCheckPasses(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed"))
	mkfile(t, root, "artifacts/archive/2026-09-02-b/plan.md", fm("done"))

	if err := runWrite(root); err != nil {
		t.Fatalf("runWrite: %v", err)
	}

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected check to pass after -write, got: %s", out.String())
	}
	if !strings.Contains(out.String(), "check passed") {
		t.Fatalf("expected a passing summary line, got: %s", out.String())
	}

	// artifacts/README.md must list the work topic under 进行中 and the
	// archive topic under 归档, and the generated plan.md block must contain
	// only the work topic (archive topics never appear there).
	readme, err := os.ReadFile(filepath.Join(root, "artifacts/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "2026-09-01-a") {
		t.Fatalf("README should list the work topic:\n%s", readme)
	}
	if !strings.Contains(string(readme), "2026-09-02-b") {
		t.Fatalf("README should list the archive topic:\n%s", readme)
	}
	plan, err := os.ReadFile(filepath.Join(root, planFile))
	if err != nil {
		t.Fatal(err)
	}
	between := extractBetween(string(plan), genBeginMarker, genEndMarker)
	if !strings.Contains(between, "2026-09-01-a") {
		t.Fatalf("plan.md generated block should contain the work topic:\n%s", between)
	}
	if strings.Contains(between, "2026-09-02-b") {
		t.Fatalf("plan.md generated block should not contain the archive topic:\n%s", between)
	}
}

func TestCheckFailsWhenGeneratedContentDrifts(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed"))
	if err := runWrite(root); err != nil {
		t.Fatal(err)
	}
	// Simulate someone hand-editing the generated README after the fact.
	readmePath := filepath.Join(root, "artifacts/README.md")
	existing, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readmePath, append(existing, []byte("\nhand-edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected check to fail when README.md no longer matches the generated content")
	}
	if !strings.Contains(out.String(), "README.md is out of date") {
		t.Fatalf("expected a README drift message, got: %s", out.String())
	}
}

func TestCheckFailsWhenIgnoreFileDrifts(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed"))
	if err := runWrite(root); err != nil {
		t.Fatal(err)
	}
	ignorePath := filepath.Join(root, "artifacts/.ignore")
	if err := os.WriteFile(ignorePath, []byte("stale contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected check to fail when .ignore no longer matches the generated content")
	}
	if !strings.Contains(out.String(), ".ignore is out of date") {
		t.Fatalf("expected an .ignore drift message, got: %s", out.String())
	}
}

func TestCheckFailsOnAmbiguousTopicEntry(t *testing.T) {
	root := newFakeRepo(t)
	// Two "done" plans, no README.md or plan.md to disambiguate.
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan-one.md", fm("done"))
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan-two.md", fm("done"))

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected check to fail on an ambiguous topic entry")
	}
	if !strings.Contains(out.String(), "no unambiguous entry") {
		t.Fatalf("expected an ambiguous-entry message, got: %s", out.String())
	}
}

func TestCheckFailsOnMalformedFrontmatter(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan.md", "---\ntitle: never closed\n\n# body\n")

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected check to fail on malformed frontmatter")
	}
	if !strings.Contains(out.String(), "never closed") {
		t.Fatalf("expected an unterminated-frontmatter message, got: %s", out.String())
	}
}

func TestCheckIgnoresPlainEvidenceFileWithoutFrontmatter(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan.md", fm("done"))
	// Plain evidence sitting next to the entry: no frontmatter, must not be
	// mistaken for a second entry candidate.
	mkfile(t, root, "artifacts/archive/2026-09-01-a/screenshot-notes.md", "just some notes\n")
	if err := runWrite(root); err != nil {
		t.Fatalf("runWrite: %v", err)
	}

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("a frontmatter-less evidence file must not break topic entry selection: %s", out.String())
	}
}

func TestCheckFailsOnBadLinkInsideArtifacts(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed")+"\nSee `artifacts/work/does-not-exist.md` for details.\n")
	if err := runWrite(root); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected check to fail on a dangling artifacts/... reference inside artifacts/")
	}
	if !strings.Contains(out.String(), "FAIL: artifacts/work/2026-09-01-a/plan.md:") || !strings.Contains(out.String(), "does-not-exist.md") {
		t.Fatalf("expected a FAIL dangling-reference message naming the bad path, got: %s", out.String())
	}
}

// The main repository and artifacts/ are versioned separately: a topic
// renamed in artifacts/ must not fail the main repository's checks, but the
// stale reference is still reported.
func TestCheckWarnsOnBadLinkFromMainRepository(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed"))
	if err := runWrite(root); err != nil {
		t.Fatal(err)
	}
	mkfile(t, root, "docs/some-note.md", "See `artifacts/work/does-not-exist.md` for details.\n")

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("a dangling reference from the main repository must not fail the check: %s", out.String())
	}
	if !strings.Contains(out.String(), "WARN: docs/some-note.md:1: dangling reference") {
		t.Fatalf("expected a WARN naming the bad reference, got: %s", out.String())
	}
}

func TestCheckSkipsWhenArtifactsNotCheckedOut(t *testing.T) {
	root := t.TempDir()
	mkfile(t, root, "docs/some-note.md", "See `artifacts/work/x/plan.md`.\n")

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !strings.Contains(out.String(), "not checked out") {
		t.Fatalf("expected a passing skip without artifacts/, got ok=%v: %s", ok, out.String())
	}
	if err := runWrite(root); err == nil || !strings.Contains(err.Error(), "not checked out") {
		t.Fatalf("-write without artifacts/ must fail clearly, got %v", err)
	}
}

func TestCheckFailsWhenMainRepositoryTracksArtifacts(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed"))
	git("add", "-f", "artifacts/work/2026-09-01-a/plan.md")

	for _, checkedOut := range []bool{false, true} {
		if checkedOut {
			mkfile(t, root, "artifacts/README.md", "placeholder\n")
		}
		var out strings.Builder
		ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
		if err != nil {
			t.Fatal(err)
		}
		if ok || !strings.Contains(out.String(), "tracks 1 file(s) under artifacts/ (first: artifacts/work/2026-09-01-a/plan.md)") {
			t.Fatalf("checkedOut=%v: expected a FAIL for the tracked file, got ok=%v: %s", checkedOut, ok, out.String())
		}
	}

	git("rm", "-q", "--cached", "artifacts/work/2026-09-01-a/plan.md")
	var out strings.Builder
	if _, err := runCheck(root, mustNow(t, "2026-09-24"), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "tracks") {
		t.Fatalf("an untracked artifacts/ checkout must not be reported: %s", out.String())
	}
}

func TestCheckWarnsButPassesOnStaleActiveDoc(t *testing.T) {
	root := newFakeRepo(t)
	stale := strings.NewReplacer("2026-09-02", "2026-08-01").Replace(fm("proposed"))
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", stale)
	if err := runWrite(root); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	// 2026-09-24 is well over 21 days after the 2026-08-01 "updated" date.
	ok, err := runCheck(root, mustNow(t, "2026-09-24"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("staleness must warn, not fail: %s", out.String())
	}
	if !strings.Contains(out.String(), "WARN") || !strings.Contains(out.String(), "stale") {
		t.Fatalf("expected a staleness WARN line, got: %s", out.String())
	}
}

func TestCheckDoesNotWarnOnFreshActiveDoc(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed")) // updated: 2026-09-02
	if err := runWrite(root); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	ok, err := runCheck(root, mustNow(t, "2026-09-10"), &out) // 8 days later
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected check to pass: %s", out.String())
	}
	if strings.Contains(out.String(), "WARN") {
		t.Fatalf("did not expect a staleness warning within the 21-day window: %s", out.String())
	}
}

func TestWriteUsesOutcomeForDoneTopicInReadme(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan.md", fmWithOutcome("done", "交付结果说明"))
	if err := runWrite(root); err != nil {
		t.Fatalf("runWrite: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(root, "artifacts/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "交付结果说明") {
		t.Fatalf("expected README to show the outcome for a done topic:\n%s", readme)
	}
	if strings.Contains(string(readme), "一句话摘要") {
		t.Fatalf("expected README to prefer outcome over summary for a done topic:\n%s", readme)
	}
}

func TestWriteFallsBackToSummaryWhenOutcomeMissingForDoneTopic(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan.md", fm("done")) // no outcome field
	if err := runWrite(root); err != nil {
		t.Fatalf("runWrite: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(root, "artifacts/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "一句话摘要") {
		t.Fatalf("expected README to fall back to summary when outcome is unset:\n%s", readme)
	}
}

func TestWriteUsesSummaryNotOutcomeForProposedTopic(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fmWithOutcome("proposed", "还没到看结论的时候"))
	if err := runWrite(root); err != nil {
		t.Fatalf("runWrite: %v", err)
	}

	readme, err := os.ReadFile(filepath.Join(root, "artifacts/README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "一句话摘要") {
		t.Fatalf("expected README to show summary for a proposed topic:\n%s", readme)
	}
	if strings.Contains(string(readme), "还没到看结论的时候") {
		t.Fatalf("expected README to ignore outcome for a proposed topic:\n%s", readme)
	}

	plan, err := os.ReadFile(filepath.Join(root, planFile))
	if err != nil {
		t.Fatal(err)
	}
	between := extractBetween(string(plan), genBeginMarker, genEndMarker)
	if !strings.Contains(between, "一句话摘要") {
		t.Fatalf("expected plan.md's generated block to show summary for a proposed topic:\n%s", between)
	}
	if strings.Contains(between, "还没到看结论的时候") {
		t.Fatalf("expected plan.md's generated block to ignore outcome for a proposed topic:\n%s", between)
	}
}

func TestWriteFailsWhenDroppedDocHasNoOutcome(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan.md", fm("dropped")) // no outcome field
	if err := runWrite(root); err == nil {
		t.Fatal("expected runWrite to fail for a dropped doc with no outcome")
	}
}

func TestWriteFailsWhenWorkTopicEntryIsDone(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("done"))
	if err := runWrite(root); err == nil {
		t.Fatal("expected runWrite to fail: a work/ topic's entry must be active or proposed")
	}
}

func TestWriteFailsWhenArchiveDocIsActive(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/archive/2026-09-01-a/plan.md", fm("done"))
	mkfile(t, root, "artifacts/archive/2026-09-01-a/nested/side.md", fm("active"))
	if err := runWrite(root); err == nil {
		t.Fatal("expected runWrite to fail: an archive/ document must not be active")
	}
}

func TestIgnoreFileListsSupersededAndDroppedNotDoneOrProposed(t *testing.T) {
	root := newFakeRepo(t)
	mkfile(t, root, "artifacts/work/2026-09-01-a/plan.md", fm("proposed"))
	superseded := strings.Replace(fm("superseded"), supersededByPlaceholder,
		"artifacts/archive/2026-09-02-b/plan-winner.md", 1)
	mkfile(t, root, "artifacts/archive/2026-09-02-b/plan-loser.md", superseded)
	mkfile(t, root, "artifacts/archive/2026-09-02-b/plan-winner.md", fm("done"))
	if err := runWrite(root); err != nil {
		t.Fatalf("runWrite: %v", err)
	}

	ignore, err := os.ReadFile(filepath.Join(root, "artifacts/.ignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ignore), "plan-loser.md") {
		t.Fatalf("expected .ignore to list the superseded doc:\n%s", ignore)
	}
	if strings.Contains(string(ignore), "plan-winner.md") {
		t.Fatalf(".ignore must not list a done doc:\n%s", ignore)
	}
	if strings.Contains(string(ignore), "2026-09-01-a") {
		t.Fatalf(".ignore must not list a proposed doc:\n%s", ignore)
	}
}

// extractBetween returns the substring strictly between begin and end,
// failing the enclosing test (via panic, caught nowhere — callers only use
// this after confirming -write succeeded) if either marker is absent.
func extractBetween(s, begin, end string) string {
	i := strings.Index(s, begin)
	j := strings.Index(s, end)
	if i < 0 || j < 0 || j < i {
		return ""
	}
	return s[i+len(begin) : j]
}
