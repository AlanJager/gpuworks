// Package native implements the image pull and container-create halves of the
// udocker driver directly in Go, against udocker's own on-disk layout.
//
// udocker spends its time in three avoidable places: it downloads layers one
// at a time through pycurl, it re-hashes every blob in Python after download,
// and `udocker create` shells out to tar twice per layer (a whiteout scan and
// the extraction) with gzip decoding serially inside each pass. On a
// multi-gigabyte engine image those passes read and write the whole image
// several times over.
//
// This package keeps udocker's directory contract exactly -- repos/<repo>/<tag>
// with a manifest, TAG, v2 marker and per-blob symlinks into layers/, and
// containers/<id> with ROOT, container.json and imagerepo.name -- so
// `udocker setup --nvidia`, `udocker setup --execmode=P1` and `udocker run`
// continue to operate on what it produces. Only the data plumbing is replaced:
// blobs download concurrently with inline sha256 verification, and layers are
// inflated and extracted straight into ROOT in one parallel pass.
package native

import "strings"

// Ref is a parsed image reference. Local and Remote differ because udocker
// stores an image under the name the user wrote (minus the tag) while the
// registry API needs the host stripped and "library/" inserted for bare Docker
// Hub names -- udocker's own _parse_imagerepo makes exactly this split.
type Ref struct {
	Registry string // registry host used for /v2/ API calls
	Remote   string // repository path on the registry, e.g. "library/ubuntu"
	Local    string // repository name in UDOCKER_DIR/repos, e.g. "ubuntu"
	Tag      string // tag, or a "sha256:..." digest reference
}

// DockerHub is the default registry for unqualified references.
const DockerHub = "registry-1.docker.io"

// ParseRef splits an image name using docker's reference convention: the first
// component is a registry host only when it contains a dot or a colon, or is
// "localhost"; a bare single-component Docker Hub name gets "library/".
func ParseRef(image string) Ref {
	ref := Ref{Registry: DockerHub, Tag: "latest"}
	rest := image
	if i := strings.Index(rest, "@"); i >= 0 { // repo@sha256:...
		ref.Tag = rest[i+1:]
		rest = rest[:i]
	} else if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i+1:], "/") {
		ref.Tag = rest[i+1:]
		rest = rest[:i]
	}
	ref.Local = rest

	components := strings.Split(rest, "/")
	if len(components) >= 2 && (strings.ContainsAny(components[0], ".:") || components[0] == "localhost") {
		ref.Registry = components[0]
		components = components[1:]
	}
	if len(components) == 1 && components[0] != "library" && ref.Registry == DockerHub {
		components = append([]string{"library"}, components...)
	}
	ref.Remote = strings.Join(components, "/")
	return ref
}

// dockerHubHosts are the names Docker treats as Docker Hub.
var dockerHubHosts = map[string]bool{DockerHub: true, "docker.io": true, "index.docker.io": true}

// FullName is the reference in Docker's fully qualified form
// (docker.io/library/alpine:3.20), which containerd image stores require.
func (r Ref) FullName() string {
	host, remote := r.Registry, r.Remote
	if dockerHubHosts[host] {
		host = "docker.io"
		if !strings.Contains(remote, "/") {
			remote = "library/" + remote
		}
	}
	sep := ":"
	if r.IsDigest() {
		sep = "@"
	}
	return host + "/" + remote + sep + r.Tag
}

// IsDigest reports whether the reference pins a digest rather than a tag.
func (r Ref) IsDigest() bool { return strings.HasPrefix(r.Tag, "sha256:") }
