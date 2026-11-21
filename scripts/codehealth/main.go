// Command codehealth reports where the tree got harder to change between two
// commits. It reads git only (no checkout, no build) and prints Markdown:
//
//   - churn: the files most merges touched, where the next conflicts are
//   - complexity: branch count, nesting and length per function, at head and
//     against base (new, grown, and the distribution)
//   - deferrals: what was added that puts a refactor off instead of doing it
//     (raised architecture budgets, lint suppressions, wording such as
//     "for now", compatibility code, build-tagged file pairs)
//
// Usage:
//
//	go run ./scripts/codehealth -base <commit> [-head <commit>] [-top 30]
//	go run ./scripts/codehealth -since "7 days ago" [-head wei/master]
//
// The numbers are for comparing one period with the next, not for gating a
// merge: scripts/check_architecture.sh holds the budgets that fail CI.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	base := flag.String("base", "", "older commit; or use -since")
	head := flag.String("head", "HEAD", "newer commit")
	since := flag.String("since", "", `take base as the last commit of head before this date, e.g. "7 days ago"`)
	top := flag.Int("top", 30, "rows per table")
	flag.Parse()
	if err := run(*base, *head, *since, *top); err != nil {
		fmt.Fprintln(os.Stderr, "codehealth:", err)
		os.Exit(1)
	}
}

func run(base, head, since string, top int) error {
	if (base == "") == (since == "") {
		return fmt.Errorf("give exactly one of -base and -since")
	}
	if since != "" {
		out, err := git("rev-list", "-1", "--before="+since, head)
		if err != nil {
			return err
		}
		if base = strings.TrimSpace(out); base == "" {
			return fmt.Errorf("no commit of %s before %q", head, since)
		}
	}
	baseID, err := resolve(base)
	if err != nil {
		return err
	}
	headID, err := resolve(head)
	if err != nil {
		return err
	}
	w := os.Stdout
	fmt.Fprintf(w, "# Code health %s..%s\n\n", baseID[:9], headID[:9])
	if err := reportChurn(w, baseID, headID, top); err != nil {
		return err
	}
	if err := reportComplexity(w, baseID, headID, top); err != nil {
		return err
	}
	return reportDeferrals(w, baseID, headID, top)
}
