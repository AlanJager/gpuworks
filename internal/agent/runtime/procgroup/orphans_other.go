//go:build !linux

package procgroup

// Without Linux's child-subreaper attribute an orphaned descendant is handed
// to the system's init, which reaps it, and this process never sees it.
func AdoptOrphans() {}

func reapOrphans(int) int { return 0 }
