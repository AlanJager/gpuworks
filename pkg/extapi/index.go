package extapi

import (
	"regexp"
	"strconv"
	"strings"
)

const objectIndex = "index"

// IndexFormat is the version of the package index this package defines.
const IndexFormat = 1

// MaxIndexEntries bounds the packages of one index; MaxIndexFiles the
// downloads of one entry.
const (
	MaxIndexEntries = 4096
	MaxIndexFiles   = 16
)

// A platform an index download is built for: "<os>_<arch>" as Go names
// them, each part in the handshake's os and arch grammar.
var downloadPlatformPattern = regexp.MustCompile(`^[a-z0-9]{2,16}_[a-z0-9]{2,16}$`)

// Index is a publisher's package index at format 1: the builds of the
// publisher's packages a server may install, with what each requires, so
// the server decides which version it can run before downloading anything.
// It travels as the payload of an Envelope signed by the publisher's
// certified key, beside the publisher's certificates, and imports offline.
//
// A reader keeps the highest Serial it applied per publisher and refuses a
// lower or equal one. Revoked and Floors name only the publisher's own
// certificates, keys, builds and packages; they apply like a revocation
// list and a trust update's floors of that publisher.
type Index struct {
	// Format is 1.
	Format    int    `json:"format"`
	Serial    uint64 `json:"serial"`
	Created   Time   `json:"created"`
	Publisher string `json:"publisher"`
	// Packages are sorted by id, then by version precedence, each build
	// once.
	Packages []IndexEntry `json:"packages"`
	Revoked  []Revocation `json:"revoked,omitempty"`
	// Floors are sorted by id.
	Floors []Floor `json:"floors,omitempty"`
}

// IndexEntry is one build of a package.
type IndexEntry struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	// Manifest is the digest of the build's manifest.json; the package a
	// download yields must carry exactly that manifest.
	Manifest Digest `json:"manifest"`
	// MinCoreHint, Protocol and Requires repeat the manifest's, for the
	// verdict before download.
	MinCoreHint string         `json:"min_core_hint,omitempty"`
	Protocol    Range          `json:"protocol"`
	Requires    map[string]int `json:"requires,omitempty"`
	// Files are the downloads of the build, sorted by platform: one per
	// platform-specific package, and one with no platform for a package
	// that holds every platform.
	Files []IndexFile `json:"files"`
	// Display is display-only data (release notes, a link).
	Display Display `json:"display,omitempty"`
}

// IndexFile is one download of a build.
type IndexFile struct {
	// Platform is "<os>_<arch>", or empty for a package for every platform.
	Platform string `json:"platform,omitempty"`
	// Path is relative to the index's own location.
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// DecodeIndex decodes and validates an index payload.
func DecodeIndex(data []byte) (*Index, error) {
	if err := checkSize(objectIndex, data, MaxEnvelopePayload); err != nil {
		return nil, err
	}
	var x Index
	if err := decodeVersioned(objectIndex, data, "format", IndexFormat, &x); err != nil {
		return nil, err
	}
	if err := x.Validate(); err != nil {
		return nil, err
	}
	return &x, nil
}

// OpenIndex verifies an index envelope against the publisher keys a reader
// trusts for the index's publisher, and decodes it. The returned key ids
// are the keys whose signatures verified.
func OpenIndex(env *Envelope, publisherKeys KeySet) (*Index, []string, error) {
	v, err := env.Verify(objectIndex, publisherKeys)
	if err != nil {
		return nil, nil, err
	}
	x, err := DecodeIndex(v.Payload)
	if err != nil {
		return nil, nil, err
	}
	return x, v.KeyIDs, nil
}

// Validate checks the index's own rules.
func (x *Index) Validate() error {
	if f := x.validate(); f != nil {
		return wireInvalid(objectIndex, f)
	}
	return nil
}

func (x *Index) validate() *fieldError {
	if x.Format != IndexFormat {
		return &fieldError{field: "format", rule: RuleEnum}
	}
	if f := first(
		inRange("serial", int64(min(x.Serial, maxSafeInteger+1)), 1, maxSafeInteger),
		requiredTime("created", x.Created),
		publisherName("publisher", x.Publisher),
		validateRevocations("revoked", x.Revoked),
	); f != nil {
		return f
	}
	if len(x.Packages) > MaxIndexEntries {
		return &fieldError{field: "packages", rule: RuleRange}
	}
	for i := range x.Packages {
		at := joinPath("packages", strconv.Itoa(i))
		if f := x.Packages[i].validate(at, x.Publisher); f != nil {
			return f
		}
		if i == 0 {
			continue
		}
		prev, cur := x.Packages[i-1], x.Packages[i]
		if prev.ID > cur.ID {
			return &fieldError{field: joinPath(at, "id"), rule: RuleOrder}
		}
		if prev.ID == cur.ID {
			switch c, _ := CompareVersions(prev.Version, cur.Version); {
			case c == 0:
				return &fieldError{field: joinPath(at, "version"), rule: RuleDuplicate}
			case c > 0:
				return &fieldError{field: joinPath(at, "version"), rule: RuleOrder}
			}
		}
	}
	for i, r := range x.Revoked {
		if r.Publisher != x.Publisher {
			return &fieldError{field: joinPath(joinPath("revoked", strconv.Itoa(i)), "publisher"), rule: RuleRelation}
		}
	}
	for i, fl := range x.Floors {
		at := joinPath("floors", strconv.Itoa(i))
		if f := first(
			matches(joinPath(at, "id"), fl.ID, packageIDPattern),
			matches(joinPath(at, "min_version"), fl.MinVersion, semverPattern),
		); f != nil {
			return f
		}
		if !idBelongsTo(x.Publisher, fl.ID) {
			return &fieldError{field: joinPath(at, "id"), rule: RuleRelation}
		}
		if i > 0 && x.Floors[i-1].ID >= fl.ID {
			rule := RuleOrder
			if x.Floors[i-1].ID == fl.ID {
				rule = RuleDuplicate
			}
			return &fieldError{field: joinPath(at, "id"), rule: rule}
		}
	}
	return nil
}

func (e *IndexEntry) validate(path, publisher string) *fieldError {
	if f := first(
		matches(joinPath(path, "id"), e.ID, packageIDPattern),
		matches(joinPath(path, "version"), e.Version, semverPattern),
		matches(joinPath(path, "manifest"), string(e.Manifest), digestPattern),
		inRange(joinPath(path, "min_core_hint"), int64(len(e.MinCoreHint)), 0, 32),
		e.Protocol.validate(joinPath(path, "protocol")),
		validateCapabilities(joinPath(path, "requires"), e.Requires),
		e.Display.validate(joinPath(path, "display")),
	); f != nil {
		return f
	}
	if !idBelongsTo(publisher, e.ID) {
		return &fieldError{field: joinPath(path, "id"), rule: RuleRelation}
	}
	if len(e.Files) == 0 {
		return &fieldError{field: joinPath(path, "files"), rule: RuleRequired}
	}
	if len(e.Files) > MaxIndexFiles {
		return &fieldError{field: joinPath(path, "files"), rule: RuleRange}
	}
	for i, file := range e.Files {
		at := joinPath(joinPath(path, "files"), strconv.Itoa(i))
		if f := first(
			optionalMatches(joinPath(at, "platform"), file.Platform, downloadPlatformPattern),
			matches(joinPath(at, "path"), file.Path, filePathPattern),
			inRange(joinPath(at, "size"), file.Size, 1, maxSafeInteger),
			matches(joinPath(at, "sha256"), file.SHA256, sha256HexPattern),
		); f != nil {
			return f
		}
		if hasDotSegment(file.Path) || !strings.HasSuffix(file.Path, ".gwpkg") {
			return &fieldError{field: joinPath(at, "path"), rule: RuleGrammar}
		}
		if i > 0 && e.Files[i-1].Platform >= file.Platform {
			rule := RuleOrder
			if e.Files[i-1].Platform == file.Platform {
				rule = RuleDuplicate
			}
			return &fieldError{field: joinPath(at, "platform"), rule: rule}
		}
	}
	return nil
}
