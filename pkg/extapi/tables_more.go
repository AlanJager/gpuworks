package extapi

import (
	"regexp"
	"strconv"
)

var (
	// A node id: the server's numeric id, never reused, in decimal.
	nodeIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
	// An attestation's format: what a reader verifies it as.
	attestationFormatPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	// A banner or attestation name.
	nameIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// NodeHoldsV1 is table.node_holds@1: nodes the owner holds from scheduling
// (maintenance, quarantine), with the reason shown on each. Holding never
// stops running work. Rows naming a node the server does not have are kept
// and match nothing.
type NodeHoldsV1 struct {
	// Nodes maps a node id, in decimal, to the hold's reason.
	Nodes map[string]Notice `json:"nodes,omitempty"`
}

func (*NodeHoldsV1) TableKind() string { return KindNodeHolds }
func (*NodeHoldsV1) TableSchema() int  { return 1 }

// Validate checks the body's rules.
func (t *NodeHoldsV1) Validate() error {
	if len(t.Nodes) > MaxNodeHolds {
		return tooLarge(KindNodeHolds, "nodes", MaxNodeHolds)
	}
	for _, id := range sortedKeys(anyMap(t.Nodes)) {
		at := joinPath("nodes", id)
		f := matches(at, id, nodeIDPattern)
		if f == nil {
			f = t.Nodes[id].validate(at)
		}
		if f != nil {
			f.row = id
			return stateRejected(KindNodeHolds, f)
		}
	}
	return nil
}

// Banner audiences and severities.
const (
	AudienceAdmin = "admin" // administrators only
	AudienceAll   = "all"   // every signed-in user

	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityDanger  = "danger"
)

// BannersV1 is table.banners@1: notices the console shows above every page.
type BannersV1 struct {
	Banners []Banner `json:"banners,omitempty"`
}

// Banner is one notice's timeline. Ids are the owner's own.
type Banner struct {
	ID       string       `json:"id"`
	Audience string       `json:"audience"`
	Steps    []BannerStep `json:"steps"`
	Display  Display      `json:"display,omitempty"`
}

// BannerStep is what the banner shows from At on: a severity and a notice,
// or nothing when Hidden.
type BannerStep struct {
	At       *Time   `json:"at,omitempty"`
	Hidden   bool    `json:"hidden,omitempty"`
	Severity string  `json:"severity,omitempty"`
	Notice   *Notice `json:"notice,omitempty"`
	Link     string  `json:"link,omitempty"`
}

func (*BannersV1) TableKind() string { return KindBanners }
func (*BannersV1) TableSchema() int  { return 1 }

// Validate checks the body's rules.
func (t *BannersV1) Validate() error {
	if len(t.Banners) > MaxBanners {
		return tooLarge(KindBanners, "banners", MaxBanners)
	}
	seen := map[string]bool{}
	for i, b := range t.Banners {
		at := joinPath("banners", strconv.Itoa(i))
		if len(b.Steps) > MaxSteps {
			return tooLarge(KindBanners, joinPath(at, "steps"), MaxSteps)
		}
		f := first(
			matches(joinPath(at, "id"), b.ID, nameIDPattern),
			oneOf(joinPath(at, "audience"), b.Audience, AudienceAdmin, AudienceAll),
			b.Display.validate(joinPath(at, "display")),
		)
		if f == nil && seen[b.ID] {
			f = &fieldError{field: joinPath(at, "id"), rule: RuleDuplicate}
		}
		seen[b.ID] = true
		if f == nil {
			f = validateTimeline(joinPath(at, "steps"), len(b.Steps), func(i int) *Time { return b.Steps[i].At })
		}
		for j := 0; f == nil && j < len(b.Steps); j++ {
			f = b.Steps[j].validate(joinPath(joinPath(at, "steps"), strconv.Itoa(j)))
		}
		if f != nil {
			f.row = b.ID
			return stateRejected(KindBanners, f)
		}
	}
	return nil
}

func (s BannerStep) validate(path string) *fieldError {
	if s.Hidden {
		if s.Severity != "" || s.Notice != nil || s.Link != "" {
			return &fieldError{field: joinPath(path, "hidden"), rule: RuleExclusive}
		}
		return nil
	}
	if s.Notice == nil {
		return &fieldError{field: joinPath(path, "notice"), rule: RuleRequired}
	}
	return first(
		oneOf(joinPath(path, "severity"), s.Severity, SeverityInfo, SeverityWarning, SeverityDanger),
		s.Notice.validate(joinPath(path, "notice")),
		optionalMatches(joinPath(path, "link"), s.Link, linkPattern),
	)
}

// AttestationsV1 is table.attestations@1: opaque statements the server
// stores and relays to the owner's publisher's other packages, and never
// interprets.
type AttestationsV1 struct {
	Attestations []Attestation `json:"attestations,omitempty"`
}

// Attestation is one opaque statement.
type Attestation struct {
	Name string `json:"name"`
	// Feature is the feature key the statement is about, if any.
	Feature string `json:"feature,omitempty"`
	// Format tells a reader how to verify Blob.
	Format string `json:"format"`
	Blob   []byte `json:"blob"`
}

func (*AttestationsV1) TableKind() string { return KindAttestations }
func (*AttestationsV1) TableSchema() int  { return 1 }

// Validate checks the body's rules.
func (t *AttestationsV1) Validate() error {
	if len(t.Attestations) > MaxAttestations {
		return tooLarge(KindAttestations, "attestations", MaxAttestations)
	}
	seen := map[string]bool{}
	for i, a := range t.Attestations {
		at := joinPath("attestations", strconv.Itoa(i))
		if len(a.Blob) > MaxAttestationBlob {
			return tooLarge(KindAttestations, joinPath(at, "blob"), MaxAttestationBlob)
		}
		f := first(
			matches(joinPath(at, "name"), a.Name, nameIDPattern),
			optionalMatches(joinPath(at, "feature"), a.Feature, featureKeyPattern),
			matches(joinPath(at, "format"), a.Format, attestationFormatPattern),
		)
		if f == nil && len(a.Blob) == 0 {
			f = &fieldError{field: joinPath(at, "blob"), rule: RuleRequired}
		}
		if f == nil && seen[a.Name] {
			f = &fieldError{field: joinPath(at, "name"), rule: RuleDuplicate}
		}
		seen[a.Name] = true
		if f != nil {
			f.row = a.Name
			return stateRejected(KindAttestations, f)
		}
	}
	return nil
}
