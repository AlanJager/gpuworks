package pullref

import "strings"

// RefKey is the identity an image reference is compared by: the reference
// spelled the way the registry resolves it (NormalizeRef), with the registry
// host and repository case folded - a host is a DNS name and a repository name
// is lowercase by definition - and the tag kept as written, since a tag is
// case-sensitive. "nginx:1", "docker.io/library/nginx:1" and
// "Docker.io/library/nginx:1" have one key; ":V1" and ":v1" have two. A blank
// reference has the empty key.
func RefKey(ref string) string {
	normalized := NormalizeRef(ref)
	if at := strings.Index(normalized, "@"); at >= 0 {
		// A digest is lowercase hex; the name before it folds like any other.
		return strings.ToLower(normalized)
	}
	slash := strings.LastIndex(normalized, "/")
	if colon := strings.LastIndex(normalized, ":"); colon > slash {
		return strings.ToLower(normalized[:colon]) + normalized[colon:]
	}
	return strings.ToLower(normalized)
}

// SameRef reports whether two image references name the same image: their
// RefKeys are equal. An empty reference is never the same as anything.
func SameRef(a, b string) bool {
	key := RefKey(a)
	return key != "" && key == RefKey(b)
}

// DeclaredFirst puts the declared references in front of chain, both in their
// own order, and keeps only the first spelling of a reference that appears more
// than once (RefKey): the order of the result is the order the references are
// tried in. Blank entries are dropped. It is how the further sources an
// inference template declares for its image (ADR 0038) precede the image's own
// chain, for the candidates a deployment offers and for the chain it falls back
// to alike.
func DeclaredFirst(declared, chain []string) []string {
	out := make([]string, 0, len(declared)+len(chain))
	seen := make(map[string]struct{}, len(declared)+len(chain))
	for _, list := range [][]string{declared, chain} {
		for _, ref := range list {
			ref = strings.TrimSpace(ref)
			key := RefKey(ref)
			if key == "" {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, ref)
		}
	}
	return out
}
