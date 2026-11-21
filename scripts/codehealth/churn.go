package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

type count struct {
	k string
	n int
}

func sortedCounts(m map[string]int) []count {
	out := make([]count, 0, len(m))
	for k, n := range m {
		out = append(out, count{k, n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].k < out[j].k
	})
	return out
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return "."
}

// reportChurn counts, per file, the landings on head's first-parent line
// that changed it and the people behind them. A file many separate landings
// change is where two branches meet.
func reportChurn(w io.Writer, base, head string, top int) error {
	out, err := git("log", "--first-parent", "--format=%H", base+".."+head)
	if err != nil {
		return err
	}
	landings := strings.Fields(out)
	files, dirs := map[string]int{}, map[string]int{}
	authors := map[string]map[string]bool{}
	for _, id := range landings {
		// The author of a merge is whoever pressed the button; the work is
		// its second parent's.
		who, err := git("log", "-1", "--format=%an", id+"^2")
		if err != nil {
			if who, err = git("log", "-1", "--format=%an", id); err != nil {
				return err
			}
		}
		who = strings.TrimSpace(who)
		names, err := git("diff", "--name-only", id+"^1", id)
		if err != nil {
			return err
		}
		seenDir := map[string]bool{}
		for _, name := range strings.Split(strings.TrimSpace(names), "\n") {
			if name == "" {
				continue
			}
			files[name]++
			if authors[name] == nil {
				authors[name] = map[string]bool{}
			}
			authors[name][who] = true
			if d := dirOf(name); !seenDir[d] {
				seenDir[d] = true
				dirs[d]++
			}
		}
	}
	fmt.Fprintf(w, "## Churn\n\n%d landings on the first-parent line.\n\n### Files most landings changed\n\n| landings | authors | file |\n|---|---|---|\n", len(landings))
	for i, kv := range sortedCounts(files) {
		if i == top {
			break
		}
		fmt.Fprintf(w, "| %d | %d | `%s` |\n", kv.n, len(authors[kv.k]), kv.k)
	}
	fmt.Fprintf(w, "\n### Directories most landings changed\n\n| landings | directory |\n|---|---|\n")
	for i, kv := range sortedCounts(dirs) {
		if i == top/2 {
			break
		}
		fmt.Fprintf(w, "| %d | `%s` |\n", kv.n, kv.k)
	}
	fmt.Fprintln(w)
	return nil
}
