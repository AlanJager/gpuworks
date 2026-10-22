package dto

// PullSourceRule is one pull source rule: images whose reference Match covers
// - or, for a site rule, addresses under the site Match names - are fetched
// from Endpoints first, in order, and from their own registry or site
// afterwards unless AllowUpstream is false. Kind ("image" or "url") is read
// from Match (an http(s) scheme names a site). NodeRequirements select the
// nodes the rule applies to (empty = every node); MatchedNodes is how many
// nodes that is now. ControlPlane: the management node reads the site
// through the rule as well (model metadata); site rules only.
type PullSourceRule struct {
	ID               uint                `json:"id"`
	Position         int                 `json:"position"`
	Kind             string              `json:"kind"`
	Match            string              `json:"match"`
	Endpoints        []string            `json:"endpoints"`
	AllowUpstream    bool                `json:"allow_upstream"`
	ControlPlane     bool                `json:"control_plane"`
	Note             string              `json:"note"`
	NodeRequirements map[string][]string `json:"node_requirements"`
	MatchedNodes     int                 `json:"matched_nodes"`
}

// PullSourceRuleRequest creates or replaces a rule. AllowUpstream is
// required: whether nodes may still reach the image's own registry is not
// something to default silently. ControlPlane defaults to off.
type PullSourceRuleRequest struct {
	Match            string              `json:"match"`
	Endpoints        []string            `json:"endpoints"`
	AllowUpstream    *bool               `json:"allow_upstream"`
	ControlPlane     bool                `json:"control_plane"`
	Note             string              `json:"note"`
	NodeRequirements map[string][]string `json:"node_requirements"`
}

// PullSourceRuleOrder is the order rules are tried in: every rule's id,
// exactly once.
type PullSourceRuleOrder struct {
	IDs []uint `json:"ids"`
}

// SyncGroup is a set of nodes among which models and images are copied
// automatically. RateLimitKiBPerSecond caps a copy between two of its nodes (0 = none).
type SyncGroup struct {
	ID                    uint                `json:"id"`
	Name                  string              `json:"name"`
	NodeRequirements      map[string][]string `json:"node_requirements"`
	RateLimitKiBPerSecond int64               `json:"rate_limit_kib_s"`
	MatchedNodes          int                 `json:"matched_nodes"`
}

// SyncGroupRequest creates or replaces a sync group.
type SyncGroupRequest struct {
	Name                  string              `json:"name"`
	NodeRequirements      map[string][]string `json:"node_requirements"`
	RateLimitKiBPerSecond int64               `json:"rate_limit_kib_s"`
}

// Credential is a stored login for one host, without its secret: a registry
// (user name and password) or a model site (a token, username empty).
type Credential struct {
	ID        uint   `json:"id"`
	Host      string `json:"host"`
	Username  string `json:"username"`
	HasSecret bool   `json:"has_secret"`
}

// CredentialRequest creates or replaces a login. Username empty is a model
// site's token. On a replace an empty Secret keeps the stored one.
type CredentialRequest struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Secret   string `json:"secret,omitempty"`
}

// CredentialLoginTest is the outcome of trying a stored login from the
// management node.
type CredentialLoginTest struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// MetadataSources are the addresses a browser reads a model's metadata at
// for the console: the ones the site rules chosen for the management node
// give (planning.ServerFetchCandidates), in order. Empty when those rules
// forbid every address of the source.
type MetadataSources struct {
	Source string   `json:"source"`
	Bases  []string `json:"bases"`
}

// SourceRuleTest asks one node whether it reaches the addresses a site rule
// being edited offers (it need not be saved), about Repo (a small public
// repository by default, or the model about to be deployed).
type SourceRuleTest struct {
	Match         string   `json:"match"`
	Endpoints     []string `json:"endpoints"`
	AllowUpstream bool     `json:"allow_upstream"`
	NodeUUID      string   `json:"node_uuid"`
	Repo          string   `json:"repo"`
}

// SourceRuleTestResult is the node's answer, address by address, in the
// order it would try them. Status is "ok", "auth_required" (the address wants
// a login: a gated repository, or a mirror that takes none), "http_error"
// (Code says what it answered) or "unreachable" (Error says why).
type SourceRuleTestResult struct {
	Source   string               `json:"source"`
	Repo     string               `json:"repo"`
	NodeUUID string               `json:"node_uuid"`
	NodeName string               `json:"node_name"`
	Outcomes []SourceProbeOutcome `json:"outcomes"`
}

type SourceProbeOutcome struct {
	Base      string `json:"base"`
	Status    string `json:"status"`
	Code      int    `json:"code,omitempty"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

// PullProxy is one proxy entry: the nodes its NodeRequirements select (empty =
// every node) send the platform's requests for them - image pulls, model
// downloads, registry probes - through it, unless an earlier entry selects
// them first. ControlPlane makes it the management node's outbound proxy too
// (the first such entry). The addresses never carry the login: Username is
// shown, the password only as HasPassword. MatchedNodes is how many nodes the
// requirements select now.
type PullProxy struct {
	ID               uint                `json:"id"`
	Position         int                 `json:"position"`
	HTTPProxy        string              `json:"http_proxy"`
	HTTPSProxy       string              `json:"https_proxy"`
	NoProxy          string              `json:"no_proxy"`
	Username         string              `json:"username"`
	HasPassword      bool                `json:"has_password"`
	ControlPlane     bool                `json:"control_plane"`
	Note             string              `json:"note"`
	NodeRequirements map[string][]string `json:"node_requirements"`
	MatchedNodes     int                 `json:"matched_nodes"`
}

// PullProxyRequest creates or replaces a proxy entry. Password, when present,
// replaces the stored one ("" removes it); left out, an update keeps it.
type PullProxyRequest struct {
	HTTPProxy        string              `json:"http_proxy"`
	HTTPSProxy       string              `json:"https_proxy"`
	NoProxy          string              `json:"no_proxy"`
	Username         string              `json:"username"`
	Password         *string             `json:"password,omitempty"`
	ControlPlane     bool                `json:"control_plane"`
	Note             string              `json:"note"`
	NodeRequirements map[string][]string `json:"node_requirements"`
}

// PullProxyOrder is the order proxy entries are matched in: every entry's
// id, exactly once.
type PullProxyOrder struct {
	IDs []uint `json:"ids"`
}

// NodePullProxy is the proxy one node uses: the entry (nil when no entry
// selects it, and the node then uses its agent's own environment) and its
// container runtime, whose daemon may need a restart to take a change.
type NodePullProxy struct {
	Proxy   *PullProxy  `json:"proxy"`
	Runtime NodeRuntime `json:"runtime"`
}

// RuntimeProxy is what a node's agent observes about the proxy its container
// runtime daemon (dockerd) pulls through, and what it can do about it.
// Applied is whether the daemon already uses the proxy the platform set for
// the node. Privilege is "root", "sudo" or "none".
type RuntimeProxy struct {
	HTTPProxy  string `json:"http_proxy"`
	HTTPSProxy string `json:"https_proxy"`
	NoProxy    string `json:"no_proxy"`
	// Configured is the proxy dockerd starts with (systemd's view); empty
	// when unknown.
	ConfiguredHTTPProxy  string `json:"configured_http_proxy"`
	ConfiguredHTTPSProxy string `json:"configured_https_proxy"`
	WrittenHTTPProxy     string `json:"written_http_proxy"`
	WrittenHTTPSProxy    string `json:"written_https_proxy"`
	WrittenNoProxy       string `json:"written_no_proxy"`
	LiveRestore          bool   `json:"live_restore"`
	DaemonJSONProxy      bool   `json:"daemon_json_proxy"`
	Systemd              bool   `json:"systemd"`
	Privilege            string `json:"privilege"`
	Applied              bool   `json:"applied"`
	Error                string `json:"error,omitempty"`
}

// NodeRuntime is the node's container runtime as the network page shows it:
// which runtime, and for a daemon runtime the proxy it uses. Proxy is nil for
// a runtime that pulls in the agent's own process (it takes the node's proxy
// on every pull) and for a node that is offline.
type NodeRuntime struct {
	Driver string        `json:"driver"`
	Proxy  *RuntimeProxy `json:"proxy,omitempty"`
}
