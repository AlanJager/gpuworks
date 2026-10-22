package store

import "time"

// PullSourceRule is one pull source rule: images whose reference Match covers
// are pulled from the rule's endpoints first, in order, and from their own
// registry afterwards unless AllowUpstream is false. The rule applies to the
// nodes its requirements select (none = every node). Position orders the
// rules; a lower position is tried first.
type PullSourceRule struct {
	ID            uint   `gorm:"primaryKey"`
	Position      int    `gorm:"index"`
	Match         string `gorm:"size:255"`
	AllowUpstream bool
	// ControlPlane: the control plane also reads a site through this rule
	// (model metadata, logos, deployment admission). The server is not a node
	// and has no labels for requirements to select, so this is an explicit
	// choice; never set on an image rule, since the server pulls no images.
	ControlPlane bool
	Note         string `gorm:"size:255"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PullSourceRuleEndpoint is one of a rule's endpoints; Position orders them
// within the rule.
type PullSourceRuleEndpoint struct {
	RuleID   uint   `gorm:"primaryKey;autoIncrement:false"`
	Position int    `gorm:"primaryKey;autoIncrement:false"`
	Endpoint string `gorm:"size:255"`
}

// PullSourceRuleRequirement is one allowed value of one key of the node
// requirements selecting the nodes a rule applies to (nodelabel.Requirements,
// as DeploymentNodeRequirement is for a deployment).
type PullSourceRuleRequirement struct {
	RuleID uint   `gorm:"primaryKey;autoIncrement:false"`
	Key    string `gorm:"primaryKey;size:128"`
	Value  string `gorm:"primaryKey;size:256"`
}

// PullProxy is one proxy entry: the nodes its requirements select (none =
// every node) send the requests the platform makes for them - image pulls,
// model downloads, registry probes - through it, unless an entry before it
// in Position order selects them first: a node has at most one proxy.
// ControlPlane makes it the management node's outbound proxy as well (the
// first such entry). The URLs never carry credentials: the user name is its
// own column and the password is encrypted with the store's secretbox.
type PullProxy struct {
	ID           uint   `gorm:"primaryKey"`
	Position     int    `gorm:"index"`
	HTTPProxy    string `gorm:"size:512"`
	HTTPSProxy   string `gorm:"size:512"`
	NoProxy      string `gorm:"size:2048"`
	Username     string `gorm:"size:255"`
	SecretCipher string `gorm:"size:2048"`
	ControlPlane bool
	Note         string `gorm:"size:255"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// PullProxyRequirement is one allowed value of one key of a proxy entry's
// node requirements.
type PullProxyRequirement struct {
	ProxyID uint   `gorm:"primaryKey;autoIncrement:false"`
	Key     string `gorm:"primaryKey;size:128"`
	Value   string `gorm:"primaryKey;size:256"`
}

// SyncGroup is a set of nodes, selected by node requirements, among which
// models and images are copied from node to node automatically. A node copies
// only from nodes it shares a group with. An installation starts with one
// group that selects every node (DefaultSyncGroupName); it is an ordinary
// group and may be edited or deleted. RateLimitKiB caps a copy between two of
// its nodes (0 = no cap).
type SyncGroup struct {
	ID           uint   `gorm:"primaryKey"`
	Name         string `gorm:"size:64"`
	RateLimitKiB int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// SyncGroupRequirement is one allowed value of one key of a sync group's node
// requirements.
type SyncGroupRequirement struct {
	GroupID uint   `gorm:"primaryKey;autoIncrement:false"`
	Key     string `gorm:"primaryKey;size:128"`
	Value   string `gorm:"primaryKey;size:256"`
}

// SourceCredential is the login for one host nodes fetch from: a registry
// (Username and password) or a model site (a token, Username empty). The
// secret is stored encrypted with the store's secretbox.
type SourceCredential struct {
	ID           uint   `gorm:"primaryKey"`
	Host         string `gorm:"size:255;uniqueIndex"`
	Username     string `gorm:"size:255"`
	SecretCipher string `gorm:"size:2048"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// registryCredentialTable is registry_credentials as migration 152 created it
// and 159 moved into source_credentials.
type registryCredentialTable struct {
	ID           uint   `gorm:"primaryKey"`
	Registry     string `gorm:"size:255;uniqueIndex"`
	Username     string `gorm:"size:255"`
	SecretCipher string `gorm:"size:2048"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (registryCredentialTable) TableName() string { return "registry_credentials" }
