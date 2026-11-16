package extapi

import (
	"strconv"
	"strings"
)

const objectManifest = "manifest"

// Manifest is a package's manifest at schema 1: manifest.json in the
// package, in canonical form, which is the exact payload of manifest.sig.
//
// Schema 1 is frozen. A field that changes what the server enforces is
// added only in a new schema, as a new type; display-only additions go in
// Display.
type Manifest struct {
	// Schema is 1.
	Schema int `json:"schema"`
	// ID is the package id (ValidPackageID), inside the publisher's names
	// (IDBelongsTo).
	ID string `json:"id"`
	// Publisher must equal the subject of the signing certificate.
	Publisher string `json:"publisher"`
	// Version is a semantic version without a leading "v".
	Version string `json:"version"`
	// MinCoreHint names the oldest server release that runs this package,
	// for messages only ("v26.12"); the verdict comes from Requires.
	MinCoreHint string `json:"min_core_hint,omitempty"`
	// Created must lie inside the signing certificate's signing window.
	Created Time `json:"created"`
	// Protocol is the range of gwx majors the package speaks.
	Protocol Range `json:"protocol"`
	// Requires maps each capability the package cannot run without to the
	// lowest version it needs. A server that lacks one refuses the package
	// as incompatible, naming the capability.
	Requires map[string]int `json:"requires,omitempty"`
	// Uses maps optional capabilities to the highest version the package
	// can use; the package negotiates down at run time.
	Uses map[string]int `json:"uses,omitempty"`
	// Roles are the privileged roles the package claims, sorted. A claim is
	// honoured only if the publisher's certificate grants the role.
	Roles []string `json:"roles,omitempty"`
	// Permissions are what the administrator approves, sorted
	// ("<verb>:<object>", see PermissionVerbs).
	Permissions []string `json:"permissions,omitempty"`
	// Features are the feature keys the package declares.
	Features []FeatureDecl `json:"features,omitempty"`
	// Routes are the package's HTTP routes below /api/v1/x/<id>.
	Routes []RouteDecl `json:"routes,omitempty"`
	// UI is the package's console bundle, if it has one.
	UI *UIDecl `json:"ui,omitempty"`
	// Node is the package's node part, if it has one.
	Node *NodeDecl `json:"node,omitempty"`
	// Runtime is how the package's server part runs, if it has one.
	Runtime *RuntimeDecl `json:"runtime,omitempty"`
	// Storage is what the package keeps in its data directory, if anything.
	Storage *StorageDecl `json:"storage,omitempty"`
	// HostSetup are the host steps the package needs, from a closed
	// vocabulary; never a script.
	HostSetup []HostSetupStep `json:"host_setup,omitempty"`
	// Codes are every product code the package can emit, sorted; each has
	// en and zh text in the package's locales.
	Codes []string `json:"codes,omitempty"`
	// Files lists every member of the package except manifest.json,
	// manifest.sig and publisher.crt, sorted by path.
	Files []FileEntry `json:"files"`
	// Display is display-only data (names, descriptions, links).
	Display Display `json:"display,omitempty"`
}

// Feature gates: who answers for a declared key.
const (
	GateNone      = "none"      // always full: a free feature of the package
	GateSelf      = "self"      // the declaring package answers for it
	GateAuthority = "authority" // only the namespace authority answers
)

// Presentations: what the console shows of an unavailable feature.
const (
	PresentationPromote = "promote" // shown as locked
	PresentationConceal = "conceal" // hidden
)

// FeatureDecl declares one feature key. A third party's keys start with its
// publisher name; the project publisher's keys have no namespace.
type FeatureDecl struct {
	Key          string `json:"key"`
	Presentation string `json:"presentation"`
	Gate         string `json:"gate"`
}

// Route access levels.
const (
	RouteAdmin        = "admin"         // administrators, by session or platform token
	RouteAdminSession = "admin_session" // administrators signed in with a session
	RouteMember       = "member"        // any signed-in user or platform token
	RoutePublic       = "public"        // no authentication
)

// Route limits.
const (
	DefaultRouteMaxBody  = 1 << 20
	MaxRouteMaxBody      = 1 << 30
	DefaultRouteTimeoutS = 60
	MaxRouteTimeoutS     = 600
)

// RouteDecl declares one HTTP route of the package, served at
// /api/v1/x/<id><pattern>.
type RouteDecl struct {
	// Method is GET, POST, PUT, PATCH or DELETE.
	Method string `json:"method"`
	// Pattern starts with "/"; chi parameters ({name}) are allowed.
	Pattern string `json:"pattern"`
	// Action is "<package id>.<verb>", what an authorizer grants.
	Action string `json:"action"`
	// Access is admin, admin_session, member or public.
	Access string `json:"access"`
	// Feature gates the route; exactly one of Feature and Ungated is set.
	Feature string `json:"feature,omitempty"`
	// Ungated marks a route no feature gates.
	Ungated bool `json:"ungated,omitempty"`
	// WriteWhenReadOnly lets a non-GET request through while Feature is
	// read-only: a write that only removes or relaxes.
	WriteWhenReadOnly bool `json:"write_when_read_only,omitempty"`
	// MaxBody bounds the request body in bytes; 0 is DefaultRouteMaxBody.
	// Above it the package needs the permission http:uploads.
	MaxBody int64 `json:"max_body,omitempty"`
	// Stream marks a streamed response (no response buffering).
	Stream bool `json:"stream,omitempty"`
	// WebSocket marks a WebSocket upgrade.
	WebSocket bool `json:"websocket,omitempty"`
	// TimeoutS bounds a request in seconds; 0 is DefaultRouteTimeoutS.
	TimeoutS int `json:"timeout_s,omitempty"`
	// AIReadable lets the assistant and MCP read a GET route.
	AIReadable bool `json:"ai_readable,omitempty"`
	// Cookies lets a public route set cookies (a single sign-on callback).
	Cookies bool `json:"cookies,omitempty"`
	// Rate limits a public route per client address.
	Rate *RouteRate `json:"rate,omitempty"`
}

// RouteRate is a token bucket per client address.
type RouteRate struct {
	PerMinute int `json:"per_minute"`
	Burst     int `json:"burst"`
}

// Console bundle modes.
const (
	UIModeModule = "module" // same-origin ES module; needs the certificate grant
	UIModeIframe = "iframe" // sandboxed frame on the apps origin
)

// UIDecl declares the package's console bundle under ui/.
type UIDecl struct {
	Mode string `json:"mode"`
	// Entry is the bundle's entry file, a member under ui/.
	Entry string `json:"entry"`
	// SDK is the range of console SDK majors the bundle runs against.
	SDK Range `json:"sdk"`
	// Nav are the menu entries the bundle adds.
	Nav []NavDecl `json:"nav,omitempty"`
}

// NavDecl is one console menu entry.
type NavDecl struct {
	ID string `json:"id"`
	// Group is the id of the console menu group the entry joins.
	Group string `json:"group"`
	// LabelKey is the locale key of the entry's label.
	LabelKey string `json:"label_key"`
	// Path is the console path the entry opens, inside the package's pages.
	Path string `json:"path"`
	// Feature is the feature whose access the entry follows; empty for none.
	Feature string `json:"feature,omitempty"`
}

// NodeDecl declares the package's node part.
type NodeDecl struct {
	// App is the node application description, a member under node/.
	App string `json:"app"`
	// Images are image archives for offline sites, members under node/images/.
	Images []string `json:"images,omitempty"`
}

// Server part lifecycles.
const (
	LifecycleResident = "resident"  // started with the server
	LifecycleOnDemand = "on_demand" // started on use, stopped when idle; owns no tables
)

// RuntimeDecl declares how the server part runs.
type RuntimeDecl struct {
	Lifecycle string `json:"lifecycle"`
	// MemMiB and CPUMilli request resources; 0 takes the server's default.
	MemMiB   int `json:"mem_mib,omitempty"`
	CPUMilli int `json:"cpu_milli,omitempty"`
	// MaxConcurrency bounds server-to-plugin calls in flight; 0 is default.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// Devices are device nodes the part needs, absolute paths under /dev,
	// sorted; host setup steps make them available.
	Devices []string `json:"devices,omitempty"`
}

// Storage kinds and replication contracts.
const (
	StorageSQLite      = "sqlite"
	StorageFiles       = "files"
	StorageHAShipped   = "shipped"      // replicated with the server's database
	StorageLeaderLocal = "leader_local" // a rebuildable cache, not replicated
)

// StorageDecl declares the package's data directory.
type StorageDecl struct {
	Kind string `json:"kind"`
	HA   string `json:"ha"`
	// StateVersion is the version of the data format this build writes. A
	// server records it as a version marker; an older package build that
	// meets newer data is refused rather than run against it.
	StateVersion int `json:"state_version"`
	// ColdCopySafe lets a backup copy the directory while the plugin is down.
	ColdCopySafe bool `json:"cold_copy_safe,omitempty"`
}

// Host setup step types.
const (
	HostSetupUSBDevice = "usb_device" // a udev rule and device access for one USB device
)

// HostSetupStep is one host step from the closed vocabulary; the server's
// installer renders it.
type HostSetupStep struct {
	Type      string `json:"type"`
	VendorID  string `json:"vendor_id"`
	ProductID string `json:"product_id"`
}

// FileEntry is one package member.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// Exec marks an executable.
	Exec bool `json:"exec,omitempty"`
}

// PeekManifestVersion reads only a manifest's schema and min_core_hint, so a
// reader can name the release it needs for a manifest it cannot decode.
func PeekManifestVersion(data []byte) (schema int, minCoreHint string, err error) {
	schema, obj, err := peekVersion(objectManifest, data, "schema")
	if err != nil {
		return 0, "", err
	}
	hint, _ := obj["min_core_hint"].(string)
	return schema, hint, nil
}

// DecodeManifest decodes and validates a manifest. A manifest at a schema
// newer than ManifestSchema fails with ErrorManifestTooNew.
func DecodeManifest(data []byte) (*Manifest, error) {
	if err := checkSize(objectManifest, data, MaxEnvelopePayload); err != nil {
		return nil, err
	}
	schema, hint, err := PeekManifestVersion(data)
	if err != nil {
		return nil, err
	}
	if schema > ManifestSchema {
		return nil, newError(ErrorManifestTooNew,
			map[string]any{"version": schema, "known": ManifestSchema, "min_core_hint": hint},
			"manifest schema %d is newer than this build reads (%d); it needs %s", schema, ManifestSchema, hint)
	}
	var m Manifest
	if err := decodeStrict(objectManifest, data, &m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate checks every rule of schema 1 that needs only the manifest
// itself. Rules that relate it to a certificate, to site policy or to the
// server's capabilities are the verifier's.
func (m *Manifest) Validate() error {
	if f := m.validate(); f != nil {
		return wireInvalid(objectManifest, f)
	}
	return nil
}

func (m *Manifest) validate() *fieldError {
	if m.Schema != ManifestSchema {
		return &fieldError{field: "schema", rule: RuleEnum}
	}
	if f := first(
		matches("id", m.ID, packageIDPattern),
		publisherName("publisher", m.Publisher),
		matches("version", m.Version, semverPattern),
		inRange("min_core_hint", int64(len(m.MinCoreHint)), 0, 32),
		requiredTime("created", m.Created),
		m.Protocol.validate("protocol"),
		validateCapabilities("requires", m.Requires),
		validateCapabilities("uses", m.Uses),
		checkSortedSet("roles", m.Roles, wordPattern),
		checkSortedSet("permissions", m.Permissions, permissionPattern),
		checkPermissionVerbs(m.Permissions),
		m.validateFeatures(),
		m.validateRoutes(),
		m.validateParts(),
		checkSortedSet("codes", m.Codes, codePattern),
		validateFiles(m.Files),
		m.Display.validate("display"),
	); f != nil {
		return f
	}
	if !idBelongsTo(m.Publisher, m.ID) {
		return &fieldError{field: "id", rule: RuleRelation}
	}
	return m.validateRequiredParts()
}

func requiredTime(path string, t Time) *fieldError {
	if t.IsZero() {
		return &fieldError{field: path, rule: RuleRequired}
	}
	return nil
}

func validateCapabilities(path string, caps map[string]int) *fieldError {
	for _, name := range sortedKeys(anyMap(caps)) {
		at := joinPath(path, name)
		if !capabilityPattern.MatchString(name) {
			return &fieldError{field: at, rule: RuleGrammar}
		}
		if f := inRange(at, int64(caps[name]), 1, maxSafeInteger); f != nil {
			return f
		}
	}
	return nil
}

func anyMap[V any](m map[string]V) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// PermissionVerbs is the closed set of permission verbs of schema 1. The
// object after the colon is a table kind (push), an event topic (events), a
// read surface (read), a service (svc) and so on; whether the server offers
// it is decided against its capabilities.
var PermissionVerbs = []string{"events", "http", "metrics", "node", "notify", "push", "read", "svc"}

func checkPermissionVerbs(perms []string) *fieldError {
	for i, p := range perms {
		verb, _, _ := strings.Cut(p, ":")
		if f := oneOf(joinPath("permissions", strconv.Itoa(i)), verb, PermissionVerbs...); f != nil {
			return f
		}
	}
	return nil
}

func (m *Manifest) validateFeatures() *fieldError {
	seen := map[string]bool{}
	for i, f := range m.Features {
		at := joinPath("features", strconv.Itoa(i))
		if p := first(
			matches(joinPath(at, "key"), f.Key, featureKeyPattern),
			oneOf(joinPath(at, "presentation"), f.Presentation, PresentationPromote, PresentationConceal),
			oneOf(joinPath(at, "gate"), f.Gate, GateNone, GateSelf, GateAuthority),
		); p != nil {
			return p
		}
		if seen[f.Key] {
			return &fieldError{field: joinPath(at, "key"), rule: RuleDuplicate}
		}
		seen[f.Key] = true
	}
	return nil
}

// declares reports whether the manifest declares feature key k.
func (m *Manifest) declares(k string) bool {
	for _, f := range m.Features {
		if f.Key == k {
			return true
		}
	}
	return false
}
