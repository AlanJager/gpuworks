package distribution

import (
	"net/http"
	"regexp"
	"strings"
)

// EditionCommunity is the edition this repository builds. An index that
// names no edition is a community index, so every index published before
// editions existed stays valid for community servers.
const EditionCommunity = "community"

var editionPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidEdition reports whether name can be an edition: the empty string
// (community) or a lowercase name.
func ValidEdition(name string) bool { return name == "" || editionPattern.MatchString(name) }

// NormalizeEdition is the edition name compares are made on: an empty name
// is the community edition.
func NormalizeEdition(name string) string {
	if name = strings.TrimSpace(name); name == "" {
		return EditionCommunity
	}
	return name
}

// SameEdition reports whether two edition names are one edition.
func SameEdition(a, b string) bool { return NormalizeEdition(a) == NormalizeEdition(b) }

// Profile is where one edition of the server reads its distribution channel
// from: the edition its indexes must name, the sources that publish them and
// the keys that sign them (ADR 0040). The zero value is Community().
//
// A separately built server passes its own profile, so its update checks,
// upgrades, notices and catalog updates all come from its own channel and an
// index published for another edition is refused rather than installed.
type Profile struct {
	// Edition is the edition every index must name; empty is community.
	Edition string
	// Sources are the base URLs the edition is published to; empty is
	// DefaultSources.
	Sources []string
	// Keys are the index signing keys the edition trusts; nil is
	// TrustedKeys().
	Keys Keys
}

// Community is the profile of the community edition: the built-in sources
// and keys.
func Community() Profile {
	return Profile{Edition: EditionCommunity, Sources: append([]string(nil), DefaultSources...), Keys: TrustedKeys()}
}

// IsZero reports whether p is the zero profile, which reads as Community().
func (p Profile) IsZero() bool { return p.Edition == "" && len(p.Sources) == 0 && p.Keys == nil }

// Client is a channel client for the profile over httpClient (nil keeps the
// distribution client's own bounded transport). Its sources are the edition's
// published ones, so update checks describe the installation to them as they
// do to the built-in sources; replacing Sources afterwards with an operator's
// mirror must clear Published.
func (p Profile) Client(httpClient *http.Client) *Client {
	if p.IsZero() {
		p = Community()
	}
	if len(p.Sources) == 0 {
		p.Sources = DefaultSources
	}
	return &Client{
		Edition:   NormalizeEdition(p.Edition),
		Sources:   append([]string(nil), p.Sources...),
		Published: true,
		Keys:      p.Keys,
		HTTP:      httpClient,
	}
}
