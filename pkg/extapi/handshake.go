package extapi

import (
	"regexp"
	"strconv"
)

// Object names of the gwx/1 lifecycle messages.
const (
	objectHandshakeRequest  = "handshake_request"
	objectHandshakeResponse = "handshake_response"
	objectHealth            = "health"
)

// The gwx/1 transport, frozen with protocol major 1.
const (
	// PathInitialize is the server's first call on the plugin's socket.
	PathInitialize = "/gwx/v1/initialize"
	// PathShutdown asks the plugin to stop.
	PathShutdown = "/gwx/v1/shutdown"
	// PathHealth is the server's periodic health probe.
	PathHealth = "/gwx/v1/health"
	// HeaderDeadline carries a call's deadline as Unix milliseconds.
	HeaderDeadline = "Gwx-Deadline-Ms"
	// EnvProtocol, EnvServeFD and EnvHostFD are set in the plugin's
	// environment: the protocol major, the descriptor on which the plugin
	// serves the server, and the one on which it calls the host API.
	EnvProtocol = "GWX_PROTOCOL"
	EnvServeFD  = "GWX_SERVE_FD"
	EnvHostFD   = "GWX_HOST_FD"
	// PathHostState prefixes the host API's push path: a plugin pushes
	// one kind with PUT PathHostState+kind.
	PathHostState = "/host/v1/state/"
	// PathHostHealth answers a plugin that probes the host API; the body
	// is a Health.
	PathHostHealth = "/host/v1/health"
	// ContentTypeJSON is the media type of every gwx/1 body.
	ContentTypeJSON = "application/json"
)

// PrincipalKeyBytes is the length of the per-spawn principal key.
const PrincipalKeyBytes = 32

// HA roles the server reports; plugins run only where it leads.
const (
	HARoleLeader = "leader" // the leader of a highly available pair
	HARoleSingle = "single" // a server without a standby
)

var (
	coreVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)([-+][0-9A-Za-z.+-]{1,64})?$`)
	installIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	platformPattern    = regexp.MustCompile(`^[a-z0-9]{2,16}$`)
)

// HandshakeRequest is the body of POST /gwx/v1/initialize, the server's
// first call to a plugin it spawned.
type HandshakeRequest struct {
	// Protocol is the range of gwx majors the server speaks.
	Protocol Range       `json:"protocol"`
	Core     CoreInfo    `json:"core"`
	Plugin   PluginGrant `json:"plugin"`
	// Held is the state the server holds for this package, per kind; a
	// plugin skips a push whose digest it already holds.
	Held  []HeldState `json:"held,omitempty"`
	Clock Clock       `json:"clock"`
	// PrincipalKey is fresh per spawn; it authenticates the principal
	// headers on proxied routes.
	PrincipalKey []byte `json:"principal_key"`
}

// CoreInfo describes the server.
type CoreInfo struct {
	Version   string `json:"version"`
	InstallID string `json:"install_id"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	HARole    string `json:"ha_role"`
	// Offer maps every capability the server offers to the versions it
	// speaks. Once offered within protocol major 1, a capability version is
	// offered by every later server.
	Offer map[string]Range `json:"offer"`
}

// PluginGrant is what the server approved for the package.
type PluginGrant struct {
	ID                 string   `json:"id"`
	Version            string   `json:"version"`
	PermissionsGranted []string `json:"permissions_granted,omitempty"`
	RolesGranted       []string `json:"roles_granted,omitempty"`
	// AuthorityNamespaces are the feature namespaces the package is the
	// authority for, sorted; "" is the bare namespace.
	AuthorityNamespaces []string `json:"authority_namespaces,omitempty"`
}

// HeldState is the server's record of one pushed table.
type HeldState struct {
	Kind       string `json:"kind"`
	Schema     int    `json:"schema"`
	Generation uint64 `json:"generation"`
	Digest     Digest `json:"digest"`
}

// Clock is the server's view of time: the wall clock and the monotone
// clock that timelines follow, which never runs backwards.
type Clock struct {
	Wall     Time `json:"wall"`
	ExtClock Time `json:"extclock"`
}

// HandshakeResponse is the plugin's answer to initialize.
type HandshakeResponse struct {
	// Protocol is the major the plugin chose from the server's range.
	Protocol int    `json:"protocol"`
	ID       string `json:"id"`
	Version  string `json:"version"`
	// ManifestSHA256 is the digest of the manifest the plugin was built
	// with; the server refuses a binary that does not belong to its package.
	ManifestSHA256 Digest `json:"manifest_sha256"`
	// Use maps each capability the plugin will use to the version it chose;
	// each must be within the server's offer.
	Use map[string]int `json:"use,omitempty"`
	// Services are the optional services the plugin serves, sorted.
	Services []string `json:"services,omitempty"`
}

// Health is the body of GET /gwx/v1/health.
type Health struct {
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
	Params Params `json:"params,omitempty"`
}

// Health statuses.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
)

// DecodeHandshakeRequest decodes and validates an initialize request. A
// request whose protocol range lacks ProtocolMajor fails with
// ErrorProtocolUnsupported.
func DecodeHandshakeRequest(data []byte) (*HandshakeRequest, error) {
	var r HandshakeRequest
	if err := decodeStrict(objectHandshakeRequest, data, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the request's own rules.
func (r *HandshakeRequest) Validate() error {
	if f := r.Protocol.validate("protocol"); f != nil {
		return wireInvalid(objectHandshakeRequest, f)
	}
	if err := checkProtocol(r.Protocol); err != nil {
		return err
	}
	if f := r.validate(); f != nil {
		return wireInvalid(objectHandshakeRequest, f)
	}
	return nil
}

func (r *HandshakeRequest) validate() *fieldError {
	c := r.Core
	if f := first(
		matches("core.version", c.Version, coreVersionPattern),
		matches("core.install_id", c.InstallID, installIDPattern),
		matches("core.os", c.OS, platformPattern),
		matches("core.arch", c.Arch, platformPattern),
		oneOf("core.ha_role", c.HARole, HARoleLeader, HARoleSingle),
		matches("plugin.id", r.Plugin.ID, packageIDPattern),
		matches("plugin.version", r.Plugin.Version, semverPattern),
		checkSortedSet("plugin.permissions_granted", r.Plugin.PermissionsGranted, permissionPattern),
		checkSortedSet("plugin.roles_granted", r.Plugin.RolesGranted, wordPattern),
		checkNamespaces("plugin.authority_namespaces", r.Plugin.AuthorityNamespaces),
		requiredTime("clock.wall", r.Clock.Wall),
		requiredTime("clock.extclock", r.Clock.ExtClock),
	); f != nil {
		return f
	}
	if len(c.Offer) == 0 {
		return &fieldError{field: "core.offer", rule: RuleRequired}
	}
	for _, name := range sortedKeys(anyMap(c.Offer)) {
		at := joinPath("core.offer", name)
		if !capabilityPattern.MatchString(name) {
			return &fieldError{field: at, rule: RuleGrammar}
		}
		if f := c.Offer[name].validate(at); f != nil {
			return f
		}
	}
	seen := map[string]bool{}
	for i, h := range r.Held {
		at := joinPath("held", strconv.Itoa(i))
		if f := first(
			matches(joinPath(at, "kind"), h.Kind, wordPattern),
			inRange(joinPath(at, "schema"), int64(h.Schema), 1, maxSafeInteger),
			inRange(joinPath(at, "generation"), int64(min(h.Generation, maxSafeInteger+1)), 1, maxSafeInteger),
			matches(joinPath(at, "digest"), string(h.Digest), digestPattern),
		); f != nil {
			return f
		}
		if seen[h.Kind] {
			return &fieldError{field: joinPath(at, "kind"), rule: RuleDuplicate}
		}
		seen[h.Kind] = true
	}
	if len(r.PrincipalKey) != PrincipalKeyBytes {
		return &fieldError{field: "principal_key", rule: RuleRange}
	}
	return nil
}

// DecodeHandshakeResponse decodes and validates an initialize answer.
func DecodeHandshakeResponse(data []byte) (*HandshakeResponse, error) {
	var r HandshakeResponse
	if err := decodeStrict(objectHandshakeResponse, data, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}

// Validate checks the answer's own rules. Whether Use fits the offer and the
// approval, and whether ManifestSHA256 is the installed manifest's, is the
// server's check.
func (r *HandshakeResponse) Validate() error {
	if r.Protocol != ProtocolMajor {
		return checkProtocol(Range{Min: r.Protocol, Max: r.Protocol})
	}
	if f := first(
		matches("id", r.ID, packageIDPattern),
		matches("version", r.Version, semverPattern),
		matches("manifest_sha256", string(r.ManifestSHA256), digestPattern),
		validateCapabilities("use", r.Use),
		checkSortedSet("services", r.Services, verbPattern),
	); f != nil {
		return wireInvalid(objectHandshakeResponse, f)
	}
	return nil
}

// DecodeHealth decodes and validates a health answer.
func DecodeHealth(data []byte) (*Health, error) {
	var h Health
	if err := decodeStrict(objectHealth, data, &h); err != nil {
		return nil, err
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return &h, nil
}

// Validate checks the health answer's rules: a degraded plugin says why.
func (h *Health) Validate() error {
	f := first(
		oneOf("status", h.Status, HealthOK, HealthDegraded),
		optionalMatches("code", h.Code, codePattern),
		h.Params.validate("params"),
	)
	if f == nil && h.Status == HealthDegraded && h.Code == "" {
		f = &fieldError{field: "code", rule: RuleRequired}
	}
	if f == nil && h.Code == "" && len(h.Params) > 0 {
		f = &fieldError{field: "params", rule: RuleRelation}
	}
	if f != nil {
		return wireInvalid(objectHealth, f)
	}
	return nil
}
