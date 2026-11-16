package extapi

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Object names, as they appear in error parameters.
const (
	objectPublisherCert = "publisher_cert"
	objectCertBundle    = "publisher_cert_bundle"
	objectTrustUpdate   = "trust_update"
	objectCRL           = "crl"
)

// Isolation levels a certificate may require at least.
const (
	IsolationLandlock = "landlock" // the plugin runs only where Landlock confines it
	IsolationNone     = "none"     // the plugin may run unconfined
)

// MaxCertBundle bounds the certificates in publisher.crt.
const MaxCertBundle = 4

// ProjectPublisher is the publisher of the project's own packages. Only its
// certificates may grant the bare feature namespace and package ids that do
// not start with "<publisher>_".
const ProjectPublisher = "gpuworks"

// RootKeyIDPrefix starts the key id of every project root. A certified key's
// id starts with "<publisher>-" instead, and never with this prefix, so a
// revocation or a trust decision by key id names one key.
const RootKeyIDPrefix = "root-"

// PublisherCert is a publisher certificate at format 1: a publisher's
// signing key and what packages signed with it may do. It travels as the
// payload of an Envelope signed by a project root (or, for a publisher the
// host operator trusts, as the operator configures).
//
// NotAfter bounds the signing window only: a package whose Created lies
// inside [NotBefore, NotAfter] stays valid after NotAfter, so installed
// packages never expire. Who issued the certificate is the key id whose
// signature verified, never a field of the payload.
type PublisherCert struct {
	// Format is 1.
	Format int `json:"format"`
	// Serial is unique among all certificates of Publisher, whichever key
	// issued them, so (Publisher, Serial) names one certificate.
	Serial uint64 `json:"serial"`
	// Publisher is the subject; manifests signed with this key must name it.
	Publisher string `json:"publisher"`
	// KeyID, Alg and PublicKey are the publisher's signing key. KeyID
	// starts with Publisher followed by "-".
	KeyID     string     `json:"keyid"`
	Alg       string     `json:"alg"`
	PublicKey []byte     `json:"public_key"`
	NotBefore Time       `json:"not_before"`
	NotAfter  Time       `json:"not_after"`
	Grants    CertGrants `json:"grants"`
	// Display is display-only data (the publisher's name, a contact link).
	Display Display `json:"display,omitempty"`
}

// CertGrants is what packages signed under a certificate may do. A grant
// not listed is not given.
type CertGrants struct {
	// IDPrefixes are the package ids the key may sign, sorted. An entry
	// ending in "_" covers every id that starts with it; any other entry
	// covers exactly that id. Except for ProjectPublisher, every entry
	// starts with "<publisher>_".
	IDPrefixes []string `json:"id_prefixes"`
	// FeatureNS are the feature namespaces its packages may declare keys
	// in, sorted: the publisher's own name, and "" (the bare namespace)
	// for ProjectPublisher only.
	FeatureNS []string `json:"feature_ns,omitempty"`
	// AuthorityID is the one package id that may answer gate:authority keys
	// of FeatureNS.
	AuthorityID string `json:"authority_id,omitempty"`
	// Roles are the privileged roles its packages may claim, sorted.
	Roles []string `json:"roles,omitempty"`
	// Tables are the privileged table kinds its packages may push, sorted.
	Tables []string `json:"tables,omitempty"`
	// UIModule lets its packages ship same-origin console modules.
	UIModule bool `json:"ui_module,omitempty"`
	// MinIsolation is the weakest isolation its packages may run under.
	MinIsolation string `json:"min_isolation"`
}

// Key returns the certified key.
func (c *PublisherCert) Key() PublicKey {
	return PublicKey{KeyID: c.KeyID, Alg: c.Alg, Key: c.PublicKey}
}

// DecodePublisherCert decodes and validates a certificate payload.
func DecodePublisherCert(data []byte) (*PublisherCert, error) {
	var c PublisherCert
	if err := decodeVersioned(objectPublisherCert, data, "format", CertFormat, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the certificate's own rules.
func (c *PublisherCert) Validate() error {
	if f := c.validate(); f != nil {
		return wireInvalid(objectPublisherCert, f)
	}
	return nil
}

func (c *PublisherCert) validate() *fieldError {
	if c.Format != CertFormat {
		return &fieldError{field: "format", rule: RuleEnum}
	}
	if f := first(
		inRange("serial", int64(min(c.Serial, maxSafeInteger+1)), 1, maxSafeInteger),
		matches("publisher", c.Publisher, publisherPattern),
		c.Key().validate(""),
		requiredTime("not_before", c.NotBefore),
		requiredTime("not_after", c.NotAfter),
	); f != nil {
		return f
	}
	if !c.NotAfter.After(c.NotBefore.Time) {
		return &fieldError{field: "not_after", rule: RuleRelation}
	}
	g := c.Grants
	if len(g.IDPrefixes) == 0 {
		return &fieldError{field: "grants.id_prefixes", rule: RuleRequired}
	}
	if f := first(
		checkSortedSet("grants.id_prefixes", g.IDPrefixes, packageIDPattern),
		checkNamespaces("grants.feature_ns", g.FeatureNS),
		optionalMatches("grants.authority_id", g.AuthorityID, packageIDPattern),
		checkSortedSet("grants.roles", g.Roles, wordPattern),
		checkSortedSet("grants.tables", g.Tables, wordPattern),
		oneOf("grants.min_isolation", g.MinIsolation, IsolationLandlock, IsolationNone),
		c.Display.validate("display"),
	); f != nil {
		return f
	}
	if f := c.validateSubject(); f != nil {
		return f
	}
	if g.AuthorityID != "" && (len(g.FeatureNS) == 0 || !g.coversID(g.AuthorityID)) {
		return &fieldError{field: "grants.authority_id", rule: RuleRelation}
	}
	return nil
}

// validateSubject keeps the key id and the grants inside the subject's own
// names, so one publisher's certificate never reaches another's packages,
// feature keys or keys.
func (c *PublisherCert) validateSubject() *fieldError {
	if !strings.HasPrefix(c.KeyID, c.Publisher+"-") || strings.HasPrefix(c.KeyID, RootKeyIDPrefix) {
		return &fieldError{field: "keyid", rule: RuleRelation}
	}
	project := c.Publisher == ProjectPublisher
	for i, p := range c.Grants.IDPrefixes {
		if !project && !strings.HasPrefix(p, c.Publisher+"_") {
			return &fieldError{field: joinPath("grants.id_prefixes", strconv.Itoa(i)), rule: RuleRelation}
		}
	}
	for i, ns := range c.Grants.FeatureNS {
		if ns != c.Publisher && (ns != "" || !project) {
			return &fieldError{field: joinPath("grants.feature_ns", strconv.Itoa(i)), rule: RuleRelation}
		}
	}
	return nil
}

// coversID reports whether the grants let the key sign package id.
func (g CertGrants) coversID(id string) bool {
	for _, p := range g.IDPrefixes {
		if id == p || strings.HasSuffix(p, "_") && strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// CoversID reports whether packages with this id may be signed under c.
func (c *PublisherCert) CoversID(id string) bool { return c.Grants.coversID(id) }

func checkNamespaces(path string, ns []string) *fieldError {
	for i, n := range ns {
		at := joinPath(path, strconv.Itoa(i))
		if n != "" && !publisherPattern.MatchString(n) {
			return &fieldError{field: at, rule: RuleGrammar}
		}
		if i > 0 && ns[i-1] == n {
			return &fieldError{field: at, rule: RuleDuplicate}
		}
		if i > 0 && ns[i-1] > n {
			return &fieldError{field: at, rule: RuleOrder}
		}
	}
	return nil
}

// DecodeCertBundle decodes publisher.crt: a JSON array of one to
// MaxCertBundle envelopes, each carrying a PublisherCert. Several
// certificates let a publisher sign with an old and a new key while it
// rotates.
func DecodeCertBundle(data []byte) ([]Envelope, error) {
	var bundle []Envelope
	if err := decodeStrict(objectCertBundle, data, &bundle); err != nil {
		return nil, err
	}
	if len(bundle) == 0 || len(bundle) > MaxCertBundle {
		return nil, wireInvalid(objectCertBundle, &fieldError{rule: RuleRange})
	}
	for i := range bundle {
		if f := bundle[i].validate(); f != nil {
			f.field = joinPath(strconv.Itoa(i), f.field)
			return nil, wireInvalid(objectCertBundle, f)
		}
	}
	return bundle, nil
}

// OpenPublisherCert verifies a certificate envelope against the issuers a
// reader trusts and decodes it. The returned key ids name the issuers.
func OpenPublisherCert(env *Envelope, issuers KeySet) (*PublisherCert, []string, error) {
	v, err := env.Verify(objectPublisherCert, issuers)
	if err != nil {
		return nil, nil, err
	}
	c, err := DecodePublisherCert(v.Payload)
	if err != nil {
		return nil, nil, err
	}
	return c, v.KeyIDs, nil
}

// decodeVersioned reads the version field named versionKey first, so an
// object at a newer version fails with ErrorSchemaTooNew rather than with
// the unknown fields that version may add, then decodes strictly.
func decodeVersioned(object string, data []byte, versionKey string, known int, v any) error {
	version, _, err := peekVersion(object, data, versionKey)
	if err != nil {
		return err
	}
	if version > known {
		return newError(ErrorSchemaTooNew, map[string]any{"object": object, "version": version, "known": known},
			"%s version %d is newer than this build reads (%d)", object, version, known)
	}
	return decodeStrict(object, data, v)
}

// peekVersion parses data and returns its integer member versionKey (0 when
// absent) and the parsed object.
func peekVersion(object string, data []byte, versionKey string) (int, map[string]any, error) {
	tree, ferr := parseTree(data)
	if ferr != nil {
		return 0, nil, malformed(object, ferr)
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return 0, nil, malformed(object, &fieldError{rule: RuleType})
	}
	raw, ok := obj[versionKey]
	if !ok {
		return 0, obj, nil
	}
	n, ok := raw.(json.Number)
	if !ok {
		return 0, nil, malformed(object, &fieldError{field: versionKey, rule: RuleType})
	}
	i, _ := safeInteger(n)
	return int(i), obj, nil
}
