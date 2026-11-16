package extapi

import (
	"strconv"
	"strings"
)

// MaxRevocations bounds the entries of one trust update or revocation list.
const MaxRevocations = 4096

// TrustUpdate is a trust update at format 1, signed by at least one project
// root the reader currently trusts. A reader keeps the highest Serial it has
// applied and refuses a lower or equal one, so an old update cannot be
// replayed. Updates import offline.
type TrustUpdate struct {
	// Format is 1.
	Format int `json:"format"`
	// Serial increases with every update.
	Serial  uint64 `json:"serial"`
	Created Time   `json:"created"`
	// AddRoots are roots to trust from now on, sorted by key id. Every
	// root key id starts with RootKeyIDPrefix.
	AddRoots []PublicKey `json:"add_roots,omitempty"`
	// RetireRoots are root key ids to stop trusting, sorted.
	RetireRoots []string `json:"retire_roots,omitempty"`
	// Revoke are revocations to apply.
	Revoke []Revocation `json:"revoke,omitempty"`
	// Floors raise the lowest installable version of packages, sorted by id.
	Floors []Floor `json:"floors,omitempty"`
}

// Revocation names what to stop trusting. Publisher is always set and
// scopes the entry; at most one of CertSerial, KeyID and Manifest narrows
// it. Publisher alone revokes every certificate of the publisher; with
// CertSerial, one certificate; with KeyID, one of the publisher's keys (the
// key id starts with "<publisher>-"); with Manifest, one package build of
// the publisher, by its manifest digest, which a reader applies only to a
// package whose manifest names Publisher.
//
// A revocation refuses new installs. It never stops an installed package by
// itself, except an authority package revoked by Manifest, which is not
// started again; Replacement then names the version to install instead.
type Revocation struct {
	Publisher   string `json:"publisher,omitempty"`
	CertSerial  uint64 `json:"cert_serial,omitempty"`
	KeyID       string `json:"keyid,omitempty"`
	Manifest    Digest `json:"manifest,omitempty"`
	Replacement string `json:"replacement,omitempty"`
	// Display is display-only data (why it was revoked).
	Display Display `json:"display,omitempty"`
}

func (r Revocation) selector() string {
	switch {
	case r.KeyID != "":
		return "keyid:" + r.KeyID
	case r.Manifest != "":
		return "manifest:" + string(r.Manifest)
	case r.CertSerial != 0:
		return "cert:" + r.Publisher + ":" + strconv.FormatUint(r.CertSerial, 10)
	}
	return "publisher:" + r.Publisher
}

func (r Revocation) validate(path string) *fieldError {
	if r.Publisher == "" {
		return &fieldError{field: joinPath(path, "publisher"), rule: RuleRequired}
	}
	set := 0
	for _, has := range []bool{r.CertSerial != 0, r.KeyID != "", r.Manifest != ""} {
		if has {
			set++
		}
	}
	if set > 1 {
		return &fieldError{field: path, rule: RuleExclusive}
	}
	if f := first(
		publisherName(joinPath(path, "publisher"), r.Publisher),
		optionalMatches(joinPath(path, "keyid"), r.KeyID, keyIDPattern),
		optionalMatches(joinPath(path, "manifest"), string(r.Manifest), digestPattern),
		optionalMatches(joinPath(path, "replacement"), r.Replacement, semverPattern),
		inRange(joinPath(path, "cert_serial"), int64(min(r.CertSerial, maxSafeInteger+1)), 0, maxSafeInteger),
		r.Display.validate(joinPath(path, "display")),
	); f != nil {
		return f
	}
	if r.KeyID != "" && !strings.HasPrefix(r.KeyID, r.Publisher+"-") {
		return &fieldError{field: joinPath(path, "keyid"), rule: RuleRelation}
	}
	if r.Replacement != "" && r.Manifest == "" {
		return &fieldError{field: joinPath(path, "replacement"), rule: RuleRelation}
	}
	return nil
}

func validateRevocations(path string, list []Revocation) *fieldError {
	if len(list) > MaxRevocations {
		return &fieldError{field: path, rule: RuleRange}
	}
	seen := map[string]bool{}
	for i, r := range list {
		at := joinPath(path, strconv.Itoa(i))
		if f := r.validate(at); f != nil {
			return f
		}
		if seen[r.selector()] {
			return &fieldError{field: at, rule: RuleDuplicate}
		}
		seen[r.selector()] = true
	}
	return nil
}

// Floor is the lowest version of a package a reader may install, upgrade
// to or roll back to. A floor only rises.
type Floor struct {
	ID         string `json:"id"`
	MinVersion string `json:"min_version"`
}

// DecodeTrustUpdate decodes and validates a trust update payload.
func DecodeTrustUpdate(data []byte) (*TrustUpdate, error) {
	var u TrustUpdate
	if err := decodeVersioned(objectTrustUpdate, data, "format", TrustUpdateFormat, &u); err != nil {
		return nil, err
	}
	if err := u.Validate(); err != nil {
		return nil, err
	}
	return &u, nil
}

// OpenTrustUpdate verifies a trust update envelope against the roots a
// reader trusts now, and decodes it.
func OpenTrustUpdate(env *Envelope, roots KeySet) (*TrustUpdate, error) {
	v, err := env.Verify(objectTrustUpdate, roots)
	if err != nil {
		return nil, err
	}
	return DecodeTrustUpdate(v.Payload)
}

// Validate checks the update's own rules.
func (u *TrustUpdate) Validate() error {
	if f := u.validate(); f != nil {
		return wireInvalid(objectTrustUpdate, f)
	}
	return nil
}

func (u *TrustUpdate) validate() *fieldError {
	if u.Format != TrustUpdateFormat {
		return &fieldError{field: "format", rule: RuleEnum}
	}
	if f := first(
		inRange("serial", int64(min(u.Serial, maxSafeInteger+1)), 1, maxSafeInteger),
		requiredTime("created", u.Created),
		checkSortedSet("retire_roots", u.RetireRoots, keyIDPattern),
	); f != nil {
		return f
	}
	for i, r := range u.RetireRoots {
		if !strings.HasPrefix(r, RootKeyIDPrefix) {
			return &fieldError{field: joinPath("retire_roots", strconv.Itoa(i)), rule: RuleRelation}
		}
	}
	if f := validateRevocations("revoke", u.Revoke); f != nil {
		return f
	}
	if len(u.AddRoots)+len(u.RetireRoots)+len(u.Revoke)+len(u.Floors) == 0 {
		return &fieldError{field: "add_roots", rule: RuleRequired}
	}
	for i, k := range u.AddRoots {
		at := joinPath("add_roots", strconv.Itoa(i))
		if f := k.validate(at); f != nil {
			return f
		}
		if !strings.HasPrefix(k.KeyID, RootKeyIDPrefix) {
			return &fieldError{field: joinPath(at, "keyid"), rule: RuleRelation}
		}
		if i > 0 && u.AddRoots[i-1].KeyID >= k.KeyID {
			rule := RuleOrder
			if u.AddRoots[i-1].KeyID == k.KeyID {
				rule = RuleDuplicate
			}
			return &fieldError{field: joinPath(at, "keyid"), rule: rule}
		}
		for _, r := range u.RetireRoots {
			if r == k.KeyID {
				return &fieldError{field: joinPath(at, "keyid"), rule: RuleExclusive}
			}
		}
	}
	for i, fl := range u.Floors {
		at := joinPath("floors", strconv.Itoa(i))
		if f := first(
			matches(joinPath(at, "id"), fl.ID, packageIDPattern),
			matches(joinPath(at, "min_version"), fl.MinVersion, semverPattern),
		); f != nil {
			return f
		}
		if i > 0 && u.Floors[i-1].ID >= fl.ID {
			rule := RuleOrder
			if u.Floors[i-1].ID == fl.ID {
				rule = RuleDuplicate
			}
			return &fieldError{field: joinPath(at, "id"), rule: rule}
		}
	}
	return nil
}

// CRL is a revocation list at format 1. A list issued under a project root
// leaves Publisher empty; a publisher may issue one about its own
// certificates, keys and package builds, naming itself in Publisher, and
// then every entry names that publisher. Lists are cumulative: a reader
// keeps the highest Serial per issuer.
type CRL struct {
	// Format is 1.
	Format    int          `json:"format"`
	Serial    uint64       `json:"serial"`
	Created   Time         `json:"created"`
	Publisher string       `json:"publisher,omitempty"`
	Revoked   []Revocation `json:"revoked"`
}

// DecodeCRL decodes and validates a revocation list payload.
func DecodeCRL(data []byte) (*CRL, error) {
	var c CRL
	if err := decodeVersioned(objectCRL, data, "format", CRLFormat, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the list's own rules.
func (c *CRL) Validate() error {
	if f := c.validate(); f != nil {
		return wireInvalid(objectCRL, f)
	}
	return nil
}

func (c *CRL) validate() *fieldError {
	if c.Format != CRLFormat {
		return &fieldError{field: "format", rule: RuleEnum}
	}
	if f := first(
		inRange("serial", int64(min(c.Serial, maxSafeInteger+1)), 1, maxSafeInteger),
		requiredTime("created", c.Created),
		optionalPublisherName("publisher", c.Publisher),
	); f != nil {
		return f
	}
	if len(c.Revoked) == 0 {
		return &fieldError{field: "revoked", rule: RuleRequired}
	}
	if f := validateRevocations("revoked", c.Revoked); f != nil {
		return f
	}
	if c.Publisher == "" {
		return nil
	}
	for i, r := range c.Revoked {
		if r.Publisher != c.Publisher {
			return &fieldError{field: joinPath(joinPath("revoked", strconv.Itoa(i)), "publisher"), rule: RuleRelation}
		}
	}
	return nil
}
