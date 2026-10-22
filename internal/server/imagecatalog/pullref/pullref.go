// Package pullref owns where a node pulls an image from: which references
// name the same image (Copies) and the order a node tries them in, with the
// pull source rules applied (PlanCandidates). It is a pure leaf package so that
// both the image catalog and the lower store layer can share one
// implementation instead of maintaining divergent copies.
package pullref

import (
	"strings"
)

const (
	SourceQuay        = "quay"
	SourceHuaweiCloud = "huaweicloud"
	SourceDirect      = "direct"
	SourceMirror      = "mirror"
	// SourceDefault is not a registry: a client that still sends it asks for
	// a plain pull, the same as sending nothing.
	SourceDefault = "default"
)

// IsArchiveURL reports whether a registry_sources entry is an image archive
// served over HTTP rather than a registry reference. Such an entry is the
// address of a tar file to download and load, so it can never be handed to a
// pull: this is the single place that distinction is made, so no consumer can
// end up trying to pull a URL as if it were a ref.
func IsArchiveURL(entry string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	return strings.HasPrefix(entry, "http://") || strings.HasPrefix(entry, "https://")
}

// Source returns a compact source label suitable for image_nodes.source and
// operator-facing audit messages.
func Source(ref, canonical string) string {
	if ref == canonical {
		return SourceDirect
	}
	host := refHost(ref)
	switch {
	case host == "quay.io":
		return SourceQuay
	case strings.Contains(host, "myhuaweicloud.com") || strings.Contains(host, "swr."):
		return SourceHuaweiCloud
	default:
		return SourceMirror
	}
}

func refHost(ref string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(strings.TrimSpace(ref), "/", 2)[0]))
}

// NormalizeRef spells a complete image reference the way the Docker registry
// resolves it: a missing registry becomes docker.io, and a single-segment name
// additionally gains the implicit library/ namespace.
func NormalizeRef(ref string) string {
	ref = strings.TrimSuffix(strings.TrimSpace(ref), "/")
	if ref == "" {
		return ""
	}
	head, _, hasSlash := strings.Cut(ref, "/")
	if !hasSlash {
		return "docker.io/library/" + ref
	}
	if !HostLike(head) {
		return "docker.io/" + ref
	}
	return ref
}

// NormalizePrefix spells a rewrite rule prefix in the same namespace as
// NormalizeRef. Unlike a complete reference, a single-segment prefix names a
// Docker Hub namespace rather than an image in library/, so no library/
// segment is inserted.
func NormalizePrefix(prefix string) string {
	prefix = strings.TrimSuffix(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	head, _, hasSlash := strings.Cut(prefix, "/")
	if !hasSlash || !HostLike(head) {
		return "docker.io/" + prefix
	}
	return prefix
}

// HostLike reports whether a leading reference segment is a registry host
// rather than a Docker Hub namespace, using the registry's own heuristic: a
// dot, a port separator, or the literal localhost.
func HostLike(segment string) bool {
	return strings.Contains(segment, ".") || strings.Contains(segment, ":") || segment == "localhost"
}
