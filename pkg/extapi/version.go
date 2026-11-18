package extapi

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ProtocolMajor is the gwx protocol major this package speaks. Major 1 is
// frozen: its envelope, handshake, error and push envelope never change.
const ProtocolMajor = 1

// Versions of the self-describing objects this package defines. A reader
// refuses a higher version (ErrorSchemaTooNew, or ErrorManifestTooNew for a
// manifest) and never removes a lower one.
const (
	ManifestSchema    = 1
	CertFormat        = 1
	TrustUpdateFormat = 1
	CRLFormat         = 1
)

// Table kinds whose rows this package defines.
const (
	KindAvailability  = "availability"
	KindFleetCapacity = "fleet_capacity"
	KindNodeHolds     = "node_holds"
	KindBanners       = "banners"
	KindAttestations  = "attestations"
)

// Capabilities a manifest's own parts imply. A manifest that has the part
// must require the capability, so a server without it refuses the package
// as incompatible instead of mishandling it.
const (
	CapRoutes    = "routes"     // the manifest declares routes
	CapUIBundle  = "ui.bundle"  // the package carries a console bundle
	CapNodeApp   = "node.app"   // the package carries a node part
	CapHostSetup = "host.setup" // the manifest declares host setup steps
	// CapHostConfig (the manifest declares settings) is with the
	// capabilities permissions name, in permission.go.
)

// tableSchemas lists, per kind, every schema version this package defines.
// A version is never removed from this list.
var tableSchemas = map[string][]int{
	KindAvailability:  {1},
	KindFleetCapacity: {1},
	KindNodeHolds:     {1},
	KindBanners:       {1},
	KindAttestations:  {1},
}

// TableCapability is the capability name of a table kind, for example
// "table.availability".
func TableCapability(kind string) string { return "table." + kind }

// TableKinds returns the table kinds this package defines, sorted.
func TableKinds() []string {
	kinds := make([]string, 0, len(tableSchemas))
	for k := range tableSchemas {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}

// TableSchemas returns the schema versions this package defines for kind,
// ascending; nil for a kind it does not define.
func TableSchemas(kind string) []int {
	return append([]int(nil), tableSchemas[kind]...)
}

// CapabilityRef names one version of a capability, written
// "<name>@<version>", for example "table.availability@1".
type CapabilityRef struct {
	Name    string
	Version int
}

func (c CapabilityRef) String() string { return c.Name + "@" + strconv.Itoa(c.Version) }

// ParseCapabilityRef parses "<name>@<version>".
func ParseCapabilityRef(s string) (CapabilityRef, error) {
	name, ver, ok := strings.Cut(s, "@")
	v, err := strconv.Atoi(ver)
	if !ok || err != nil || v < 1 || strconv.Itoa(v) != ver || !capabilityPattern.MatchString(name) {
		return CapabilityRef{}, fmt.Errorf("capability reference %q is not <name>@<version>", s)
	}
	return CapabilityRef{Name: name, Version: v}, nil
}

// Range is an inclusive range of versions.
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Contains reports whether v lies in r.
func (r Range) Contains(v int) bool { return r.Min <= v && v <= r.Max }

func (r Range) validate(path string) *fieldError {
	return first(
		inRange(joinPath(path, "min"), int64(r.Min), 1, maxSafeInteger),
		inRange(joinPath(path, "max"), int64(r.Max), int64(max(r.Min, 1)), maxSafeInteger),
	)
}

// checkProtocol refuses a peer range without ProtocolMajor.
func checkProtocol(r Range) error {
	if r.Contains(ProtocolMajor) {
		return nil
	}
	return newError(ErrorProtocolUnsupported,
		map[string]any{"min": r.Min, "max": r.Max, "supported": ProtocolMajor},
		"peer speaks gwx/%d..%d, this build speaks gwx/%d", r.Min, r.Max, ProtocolMajor)
}

// Marker is a persisted version marker: the format version of one kind of
// stored state (pushed state's canonical form, a plugin's data) and the
// server version that wrote it. A server that finds a marker newer than it
// knows refuses to start instead of dropping or rewriting the state.
type Marker struct {
	// Name is the marker's name, a capability-style name such as
	// "pstate.availability" or "plugin.acme_backup.data".
	Name string
	// Version is the format version the state is written in.
	Version int
	// WrittenBy is the server version that wrote it, for example "v26.12.0".
	WrittenBy string
}

// PluginMarkerPrefix starts the name of every marker a package's data
// carries; every other marker is the core's own.
const PluginMarkerPrefix = "plugin."

// PluginDataMarker is the name of the marker of package id's data, whose
// version is the storage.state_version of the build that wrote it.
func PluginDataMarker(id string) string { return PluginMarkerPrefix + id + ".data" }

// TooNew returns the stored markers newer than known (marker name to the
// highest version this build reads), sorted by name.
//
// known holds the core's own markers and, for each installed package the
// caller checks, PluginDataMarker(id) at the package's storage.state_version.
// A core marker missing from known names state a newer core added, so it is
// newer than this build. A package marker missing from known belongs to a
// package the caller did not list (no longer installed, or one whose data
// its own plugin host checks before starting it), so it is not the core's to
// refuse.
func TooNew(stored []Marker, known map[string]int) []Marker {
	var out []Marker
	for _, m := range stored {
		v, listed := known[m.Name]
		if !listed && strings.HasPrefix(m.Name, PluginMarkerPrefix) {
			continue
		}
		if m.Version > v {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// CheckMarkers returns ErrorStateTooNew when any stored marker is newer than
// known. The error names the first such marker and, in needs, the newest
// server version among the writers of all of them: the version to run
// instead of this one.
func CheckMarkers(stored []Marker, known map[string]int) error {
	newer := TooNew(stored, known)
	if len(newer) == 0 {
		return nil
	}
	needs := ""
	for _, m := range newer {
		if needs == "" {
			needs = m.WrittenBy
			continue
		}
		if c, err := CompareVersions(m.WrittenBy, needs); err == nil && c > 0 {
			needs = m.WrittenBy
		}
	}
	m := newer[0]
	return newError(ErrorStateTooNew,
		map[string]any{"marker": m.Name, "version": m.Version, "known": known[m.Name], "needs": needs, "count": len(newer)},
		"stored %s is at version %d but this build reads up to %d; run %s or newer", m.Name, m.Version, known[m.Name], needs)
}

// CompareVersions compares two semantic versions by semver 2.0.0
// precedence, ignoring a leading "v" and build metadata. It returns -1, 0
// or +1.
func CompareVersions(a, b string) (int, error) {
	pa, err := parseSemver(a)
	if err != nil {
		return 0, err
	}
	pb, err := parseSemver(b)
	if err != nil {
		return 0, err
	}
	for i := range 3 {
		if c := compareNumeric(pa.core[i], pb.core[i]); c != 0 {
			return c, nil
		}
	}
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0, nil
	case len(pa.pre) == 0:
		return 1, nil
	case len(pb.pre) == 0:
		return -1, nil
	}
	for i := 0; i < len(pa.pre) && i < len(pb.pre); i++ {
		if c := comparePre(pa.pre[i], pb.pre[i]); c != 0 {
			return c, nil
		}
	}
	return compareInt(len(pa.pre), len(pb.pre)), nil
}

type semver struct {
	core [3]string
	pre  []string
}

func parseSemver(s string) (semver, error) {
	v := strings.TrimPrefix(s, "v")
	if !semverPattern.MatchString(v) {
		return semver{}, fmt.Errorf("%q is not a semantic version", s)
	}
	v, _, _ = strings.Cut(v, "+")
	v, pre, hasPre := strings.Cut(v, "-")
	parts := strings.SplitN(v, ".", 3)
	out := semver{core: [3]string{parts[0], parts[1], parts[2]}}
	if hasPre {
		out.pre = strings.Split(pre, ".")
	}
	return out, nil
}

func isNumeric(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// compareNumeric compares decimal strings without leading zeros of any
// length.
func compareNumeric(a, b string) int {
	if c := compareInt(len(a), len(b)); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

func comparePre(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		return compareNumeric(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}

func compareInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
