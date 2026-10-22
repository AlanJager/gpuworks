package native

import (
	"archive/tar"
	"fmt"
	"path"
	"strings"
)

// maxArchiveLinkHops bounds how many links one member may pass through, so a
// cycle fails instead of looping. Docker never chains them; the bound only has
// to be generous for hand-repacked archives.
const maxArchiveLinkHops = 40

// archiveLinks maps a symlink or hard-link member to the archive path it
// points at, or to "" when the target lies outside the archive. Docker before
// 25 stores a layer that appears twice once and writes the second
// <id>/layer.tar as a symlink to ../<other-id>/layer.tar; GNU tar -h turns
// those into hard links when an archive is repacked.
type archiveLinks map[string]string

// add records a link member named name. A symlink target is relative to the
// link's directory, a hard-link target is an archive path; either way only a
// path inside the archive is kept, nothing is looked up on the filesystem.
func (links archiveLinks) add(header *tar.Header, name string) {
	target := strings.TrimPrefix(header.Linkname, "./")
	if header.Typeflag == tar.TypeSymlink && !path.IsAbs(target) {
		target = path.Join(path.Dir(name), target)
	}
	target = path.Clean(target)
	if path.IsAbs(target) || target == ".." || strings.HasPrefix(target, "../") {
		target = ""
	}
	links[name] = target
}

// resolve points every link at its final target's blob in files, the same
// blob and digest as the member it names. Links that cannot be resolved are
// returned with the reason, and only fail a load when the image uses them:
// Docker would never write a dangling link, but an unused one is harmless.
func (links archiveLinks) resolve(files map[string]*archiveBlob) (map[string]error, error) {
	broken := map[string]error{}
	for name := range links {
		blob, err := links.follow(files, name)
		if err != nil {
			broken[name] = err
			continue
		}
		if hexName, ok := strings.CutPrefix(name, "blobs/sha256/"); ok && "sha256:"+hexName != blob.digest {
			return nil, fmt.Errorf("archive blob %s is corrupt: it links to content with digest %s", name, blob.digest)
		}
		files[name] = blob
	}
	return broken, nil
}

func (links archiveLinks) follow(files map[string]*archiveBlob, name string) (*archiveBlob, error) {
	current := name
	for range maxArchiveLinkHops {
		target, isLink := links[current]
		if !isLink {
			if blob, ok := files[current]; ok {
				return blob, nil
			}
			return nil, fmt.Errorf("a link to missing member %s", current)
		}
		if target == "" {
			return nil, fmt.Errorf("a link that points outside the archive")
		}
		if blob, ok := files[target]; ok {
			return blob, nil
		}
		current = target
	}
	return nil, fmt.Errorf("a link that passes through more than %d links (a cycle)", maxArchiveLinkHops)
}
