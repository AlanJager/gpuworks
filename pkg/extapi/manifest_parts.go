package extapi

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	verbPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31}){0,2}$`)
	// A locale key: "x.<id>.nav.backups".
	localeKeyPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9_]{0,63}(\.[A-Za-z0-9_]{1,64}){0,7}$`)
	devicePattern    = regexp.MustCompile(`^/dev/[A-Za-z0-9_][A-Za-z0-9_./-]{0,126}$`)
)

// Members that are the signature and trust material themselves, never listed.
var unlistedMembers = []string{"manifest.json", "manifest.sig", "publisher.crt"}

func (m *Manifest) validateRoutes() *fieldError {
	seen := map[string]bool{}
	for i, r := range m.Routes {
		at := joinPath("routes", strconv.Itoa(i))
		if f := first(
			oneOf(joinPath(at, "method"), r.Method, "GET", "POST", "PUT", "PATCH", "DELETE"),
			matches(joinPath(at, "pattern"), r.Pattern, routePatternPattern),
			matches(joinPath(at, "action"), r.Action, verbPattern),
			oneOf(joinPath(at, "access"), r.Access, RouteAdmin, RouteAdminSession, RouteMember, RoutePublic),
			optionalMatches(joinPath(at, "feature"), r.Feature, featureKeyPattern),
			inRange(joinPath(at, "max_body"), r.MaxBody, 0, MaxRouteMaxBody),
			inRange(joinPath(at, "timeout_s"), int64(r.TimeoutS), 0, MaxRouteTimeoutS),
		); f != nil {
			return f
		}
		verb, ok := strings.CutPrefix(r.Action, m.ID+".")
		if !ok || verb == "" {
			return &fieldError{field: joinPath(at, "action"), rule: RuleRelation}
		}
		if (r.Feature == "") == !r.Ungated {
			return &fieldError{field: joinPath(at, "feature"), rule: RuleExclusive}
		}
		if r.Feature != "" && !m.declares(r.Feature) {
			return &fieldError{field: joinPath(at, "feature"), rule: RuleRelation}
		}
		if r.WriteWhenReadOnly && (r.Method == "GET" || r.Feature == "") {
			return &fieldError{field: joinPath(at, "write_when_read_only"), rule: RuleRelation}
		}
		if r.AIReadable && r.Method != "GET" {
			return &fieldError{field: joinPath(at, "ai_readable"), rule: RuleRelation}
		}
		if r.WebSocket && (r.Method != "GET" || r.Stream) {
			return &fieldError{field: joinPath(at, "websocket"), rule: RuleRelation}
		}
		if r.Cookies && r.Access != RoutePublic {
			return &fieldError{field: joinPath(at, "cookies"), rule: RuleRelation}
		}
		if r.MaxBody > DefaultRouteMaxBody && !slices.Contains(m.Permissions, "http:uploads") {
			return &fieldError{field: joinPath(at, "max_body"), rule: RuleRelation}
		}
		if r.Rate != nil {
			if r.Access != RoutePublic {
				return &fieldError{field: joinPath(at, "rate"), rule: RuleRelation}
			}
			if f := first(
				inRange(joinPath(at, "rate.per_minute"), int64(r.Rate.PerMinute), 1, 100000),
				inRange(joinPath(at, "rate.burst"), int64(r.Rate.Burst), 1, 100000),
			); f != nil {
				return f
			}
		}
		key := r.Method + " " + r.Pattern
		if seen[key] {
			return &fieldError{field: joinPath(at, "pattern"), rule: RuleDuplicate}
		}
		seen[key] = true
	}
	return nil
}

func (m *Manifest) validateParts() *fieldError {
	if u := m.UI; u != nil {
		if f := first(
			oneOf("ui.mode", u.Mode, UIModeModule, UIModeIframe),
			memberUnder("ui.entry", u.Entry, "ui/"),
			m.listed("ui.entry", u.Entry),
			u.SDK.validate("ui.sdk"),
		); f != nil {
			return f
		}
		ids := map[string]bool{}
		for i, n := range u.Nav {
			at := joinPath("ui.nav", strconv.Itoa(i))
			if f := first(
				matches(joinPath(at, "id"), n.ID, wordPattern),
				matches(joinPath(at, "group"), n.Group, wordPattern),
				matches(joinPath(at, "label_key"), n.LabelKey, localeKeyPattern),
				matches(joinPath(at, "path"), n.Path, consolePathPattern),
				optionalMatches(joinPath(at, "feature"), n.Feature, featureKeyPattern),
			); f != nil {
				return f
			}
			if ids[n.ID] {
				return &fieldError{field: joinPath(at, "id"), rule: RuleDuplicate}
			}
			ids[n.ID] = true
			if n.Feature != "" && !m.declares(n.Feature) {
				return &fieldError{field: joinPath(at, "feature"), rule: RuleRelation}
			}
		}
	}
	if n := m.Node; n != nil {
		if f := first(memberUnder("node.app", n.App, "node/"), m.listed("node.app", n.App)); f != nil {
			return f
		}
		for i, img := range n.Images {
			at := joinPath("node.images", strconv.Itoa(i))
			if f := first(memberUnder(at, img, "node/images/"), m.listed(at, img)); f != nil {
				return f
			}
		}
	}
	if r := m.Runtime; r != nil {
		if f := first(
			oneOf("runtime.lifecycle", r.Lifecycle, LifecycleResident, LifecycleOnDemand),
			inRange("runtime.mem_mib", int64(r.MemMiB), 0, 1<<20),
			inRange("runtime.cpu_milli", int64(r.CPUMilli), 0, 1<<20),
			inRange("runtime.max_concurrency", int64(r.MaxConcurrency), 0, 1024),
			checkSortedSet("runtime.devices", r.Devices, devicePattern),
		); f != nil {
			return f
		}
	}
	if s := m.Storage; s != nil {
		if f := first(
			oneOf("storage.kind", s.Kind, StorageSQLite, StorageFiles),
			oneOf("storage.ha", s.HA, StorageHAShipped, StorageLeaderLocal),
			inRange("storage.state_version", int64(s.StateVersion), 1, maxSafeInteger),
		); f != nil {
			return f
		}
	}
	for i, h := range m.HostSetup {
		at := joinPath("host_setup", strconv.Itoa(i))
		if f := first(
			oneOf(joinPath(at, "type"), h.Type, HostSetupUSBDevice),
			matches(joinPath(at, "vendor_id"), h.VendorID, usbIDPattern),
			matches(joinPath(at, "product_id"), h.ProductID, usbIDPattern),
		); f != nil {
			return f
		}
		for _, prev := range m.HostSetup[:i] {
			if prev == h {
				return &fieldError{field: at, rule: RuleDuplicate}
			}
		}
	}
	return nil
}

// validateRequiredParts makes every part name the capability it needs, so a
// server without it refuses the package by capability.
func (m *Manifest) validateRequiredParts() *fieldError {
	needs := []struct {
		has bool
		cap string
	}{
		{len(m.Routes) > 0, CapRoutes},
		{m.UI != nil, CapUIBundle},
		{m.Node != nil, CapNodeApp},
		{len(m.HostSetup) > 0, CapHostSetup},
	}
	for _, n := range needs {
		if n.has && m.Requires[n.cap] == 0 {
			return &fieldError{field: joinPath("requires", n.cap), rule: RuleRequired}
		}
	}
	for name := range m.Uses {
		if m.Requires[name] != 0 {
			return &fieldError{field: joinPath("uses", name), rule: RuleExclusive}
		}
	}
	return nil
}

// listed reports a member a part names but Files does not list.
func (m *Manifest) listed(path, member string) *fieldError {
	for _, f := range m.Files {
		if f.Path == member {
			return nil
		}
	}
	return &fieldError{field: path, rule: RuleRelation}
}

func memberUnder(path, member, dir string) *fieldError {
	if f := matches(path, member, filePathPattern); f != nil {
		return f
	}
	if !strings.HasPrefix(member, dir) {
		return &fieldError{field: path, rule: RuleRelation}
	}
	return nil
}

func validateFiles(files []FileEntry) *fieldError {
	if len(files) == 0 {
		return &fieldError{field: "files", rule: RuleRequired}
	}
	for i, f := range files {
		at := joinPath("files", strconv.Itoa(i))
		if p := first(
			matches(joinPath(at, "path"), f.Path, filePathPattern),
			inRange(joinPath(at, "size"), f.Size, 0, maxSafeInteger),
			matches(joinPath(at, "sha256"), f.SHA256, sha256HexPattern),
		); p != nil {
			return p
		}
		if slices.Contains(unlistedMembers, f.Path) || hasDotSegment(f.Path) {
			return &fieldError{field: joinPath(at, "path"), rule: RuleGrammar}
		}
		if i > 0 && files[i-1].Path == f.Path {
			return &fieldError{field: joinPath(at, "path"), rule: RuleDuplicate}
		}
		if i > 0 && files[i-1].Path > f.Path {
			return &fieldError{field: joinPath(at, "path"), rule: RuleOrder}
		}
	}
	return nil
}

func hasDotSegment(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}
