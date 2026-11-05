package modelrepo

import "strings"

// IsCommitRevision reports whether a revision names a commit (hex, at least
// seven digits) rather than a branch or tag: the only kind of revision that
// pins a repository's content.
func IsCommitRevision(revision string) bool {
	revision = strings.TrimSpace(revision)
	if len(revision) < 7 {
		return false
	}
	for _, r := range strings.ToLower(revision) {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
