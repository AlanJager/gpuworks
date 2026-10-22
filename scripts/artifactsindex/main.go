// Command artifactsindex keeps artifacts/README.md, artifacts/.ignore, and
// the generated block in artifacts/plan.md in sync with the YAML
// frontmatter carried by the entry document of every topic under
// artifacts/{work,archive,research} (see scope.go and topics.go), and
// checks that "artifacts/..." references across the repo resolve to real
// paths.
//
// artifacts/ is a separate private repository checked out inside the main
// checkout (see checkout.go). When it is not checked out, check mode only
// verifies that the main repository tracks nothing under it.
//
// Usage:
//
//	go run ./scripts/artifactsindex -write   # regenerate the indexes
//	go run ./scripts/artifactsindex          # check mode (default); also -check
//
// check-time-policy (scripts/check_time_policy.sh) only scans
// internal/server/store and internal/server/api, so it does not reach this
// package, but the staleness check still avoids a bare time.Now() call sitting
// in the middle of the logic: "now" is a parameter (staleNow), defaulted to
// time.Now().UTC() in main and overridable with -now so tests are
// deterministic without faking the clock.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const staleAfter = 21 * 24 * time.Hour

func main() {
	write := flag.Bool("write", false, "regenerate artifacts/README.md, artifacts/.ignore, and the generated block in artifacts/plan.md")
	check := flag.Bool("check", false, "verify frontmatter, generated content, and artifacts/... links (default mode)")
	repoRoot := flag.String("repo", ".", "repository root")
	nowFlag := flag.String("now", "", "override 'now' for staleness warnings, as YYYY-MM-DD (default: real UTC date)")
	flag.Parse()

	if *write && *check {
		fmt.Fprintln(os.Stderr, "artifactsindex: -write and -check are mutually exclusive")
		os.Exit(2)
	}

	root, err := filepath.Abs(*repoRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "artifactsindex:", err)
		os.Exit(2)
	}

	now := time.Now().UTC()
	if *nowFlag != "" {
		now, err = time.Parse("2006-01-02", *nowFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, "artifactsindex: -now:", err)
			os.Exit(2)
		}
	}

	if *write {
		if err := runWrite(root); err != nil {
			fmt.Fprintln(os.Stderr, "artifactsindex:", err)
			os.Exit(1)
		}
		return
	}
	ok, err := runCheck(root, now, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "artifactsindex:", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
}

// loaded bundles everything discovered from one scan of the repository:
// every parsed frontmatter document, and the topics they group into. Both
// docs and topics are returned even when errs is non-empty (a topic with an
// ambiguous entry still appears, just without a usable Entry) so a single
// run can report every problem at once; callers that need a clean load
// (runWrite) must check errs is empty first.
type loaded struct {
	docs    []Doc
	topics  []Topic
	mdFiles []string
}

func loadAll(root string) (result loaded, errs []error) {
	mdFiles, err := scopeFiles(root)
	if err != nil {
		return loaded{}, []error{err}
	}

	var docs []Doc
	for _, p := range mdFiles {
		d, isDoc, err := parseDoc(root, p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !isDoc {
			continue
		}
		docs = append(docs, d)
	}

	known := map[string]bool{}
	for _, d := range docs {
		known[d.RelPath] = true
	}
	for _, d := range docs {
		errs = append(errs, validateDoc(d, known)...)
	}

	topics, err := buildTopics(root, mdFiles, docs)
	if err != nil {
		errs = append(errs, err)
		return loaded{docs: docs, mdFiles: mdFiles}, errs
	}
	errs = append(errs, validateTopics(topics)...)

	return loaded{docs: docs, topics: topics, mdFiles: mdFiles}, errs
}

func runWrite(root string) error {
	if !artifactsCheckedOut(root) {
		return fmt.Errorf("%s is not checked out; clone the artifacts repository there first", artifactsDir)
	}
	l, errs := loadAll(root)
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintln(os.Stderr, "artifactsindex:", e)
		}
		return fmt.Errorf("%d error(s); fix them before -write", len(errs))
	}
	topics := topicsWithEntries(l.topics)

	readme, err := generateArtifactsReadme(topics)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts/README.md"), []byte(readme), 0o644); err != nil {
		return err
	}

	ignore, err := generateIgnoreFile(l.docs)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts/.ignore"), []byte(ignore), 0o644); err != nil {
		return err
	}

	section, err := generatePlanSection(topics)
	if err != nil {
		return err
	}
	planPath := filepath.Join(root, planFile)
	planContent, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	spliced, err := spliceGenerated(string(planContent), section)
	if err != nil {
		return err
	}
	if err := os.WriteFile(planPath, []byte(spliced), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote artifacts/README.md, artifacts/.ignore, and artifacts/plan.md (%d documents, %d topics)\n", len(l.docs), len(topics))
	return nil
}

// runCheck runs every check and prints its findings to out. It returns
// ok=false if any check failed (files tracked under artifacts/ by the main
// repository, frontmatter/topic errors, generated-content drift, or a
// dangling link inside artifacts/); staleness and dangling links from the
// main repository into artifacts/ are warn-only and never affect ok.
func runCheck(root string, now time.Time, out io.Writer) (bool, error) {
	ok := true

	tracked, err := trackedUnderArtifacts(root)
	if err != nil {
		return false, err
	}
	if len(tracked) > 0 {
		fmt.Fprintf(out, "FAIL: the main repository tracks %d file(s) under %s/ (first: %s); %s/ is a separate repository, run: git rm -r --cached %s\n",
			len(tracked), artifactsDir, tracked[0], artifactsDir, artifactsDir)
		ok = false
	}
	if !artifactsCheckedOut(root) {
		if ok {
			fmt.Fprintf(out, "artifactsindex: %s/ is not checked out (private repository); index and link checks skipped\n", artifactsDir)
		}
		return ok, nil
	}

	l, errs := loadAll(root)
	for _, e := range errs {
		fmt.Fprintln(out, "FAIL:", e)
		ok = false
	}
	if len(errs) > 0 {
		// Frontmatter/topic structure is broken; generated-content
		// comparison would just be noise (it can't be regenerated
		// correctly), so stop here.
		return false, nil
	}
	topics := topicsWithEntries(l.topics)

	if drift, err := checkGeneratedContent(root, topics, l.docs); err != nil {
		return false, err
	} else if drift != "" {
		fmt.Fprintln(out, "FAIL:", drift)
		ok = false
	}

	issues, err := checkLinks(root, l.mdFiles)
	if err != nil {
		return false, err
	}
	for _, iss := range issues {
		if inArtifacts(iss.File) {
			fmt.Fprintln(out, "FAIL:", iss)
			ok = false
			continue
		}
		// The main repository and artifacts/ are versioned separately, so a
		// topic moved or renamed there must not break the main repository's
		// checks; the reference is still reported so it gets fixed.
		fmt.Fprintln(out, "WARN:", iss)
	}

	for _, d := range l.docs {
		if d.Status != "active" && d.Status != "proposed" {
			continue
		}
		updated, err := time.Parse("2006-01-02", d.Updated)
		if err != nil {
			continue // already reported by validateDoc
		}
		if now.Sub(updated) > staleAfter {
			fmt.Fprintf(out, "WARN: %s: status=%s, updated %s, stale for %d day(s)\n",
				d.RelPath, d.Status, d.Updated, int(now.Sub(updated).Hours()/24))
		}
	}

	if ok {
		fmt.Fprintf(out, "artifactsindex: check passed (%d documents, %d topics)\n", len(l.docs), len(topics))
	}
	return ok, nil
}

// checkGeneratedContent regenerates artifacts/README.md, artifacts/.ignore,
// and the artifacts/plan.md section in memory and compares them against what is on
// disk. It returns a human-readable description of the first drift found,
// or "" if all three match.
func checkGeneratedContent(root string, topics []Topic, docs []Doc) (string, error) {
	readme, err := generateArtifactsReadme(topics)
	if err != nil {
		return "", err
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "artifacts/README.md"))
	if err != nil {
		return "", err
	}
	if string(onDisk) != readme {
		return "artifacts/README.md is out of date; run: go run ./scripts/artifactsindex -write", nil
	}

	ignore, err := generateIgnoreFile(docs)
	if err != nil {
		return "", err
	}
	ignoreOnDisk, err := os.ReadFile(filepath.Join(root, "artifacts/.ignore"))
	if err != nil {
		return "", err
	}
	if string(ignoreOnDisk) != ignore {
		return "artifacts/.ignore is out of date; run: go run ./scripts/artifactsindex -write", nil
	}

	section, err := generatePlanSection(topics)
	if err != nil {
		return "", err
	}
	planPath := filepath.Join(root, planFile)
	planOnDisk, err := os.ReadFile(planPath)
	if err != nil {
		return "", err
	}
	spliced, err := spliceGenerated(string(planOnDisk), section)
	if err != nil {
		return "", err
	}
	if spliced != string(planOnDisk) {
		return "artifacts/plan.md's generated block is out of date; run: go run ./scripts/artifactsindex -write", nil
	}
	return "", nil
}
