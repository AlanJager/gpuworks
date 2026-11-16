package extapi

import (
	"bytes"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
)

// Grammars shared by the wire objects. Each is part of the frozen format.
var (
	// A package id: also the route segment /api/v1/x/<id>/ and a directory
	// name. A third party's ids start with "<publisher>_"; which ids a
	// publisher may sign is a grant of its certificate, whose entries share
	// this grammar (an entry ending in "_" grants every id it starts).
	packageIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	// A publisher name. It is also the feature namespace of a third party's
	// keys, so it is a valid feature key segment, and it has no underscore,
	// so "<publisher>_" splits a package id unambiguously.
	publisherPattern = regexp.MustCompile(`^[a-z][a-z0-9]{1,15}$`)
	// A feature key: up to three dot-separated segments. A third party's
	// keys start with its publisher name as the first segment.
	featureKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}(\.[a-z][a-z0-9_]{1,31}){0,2}$`)
	// A product code (ADR 0067).
	codePattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*(_[A-Z0-9]+)+$`)
	// A key id: names exactly one public key, forever. A project root's key
	// id starts with RootKeyIDPrefix and a certified key's with
	// "<publisher>-", so the two never collide.
	keyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)
	// A capability name: "table.availability", "host.fleet", "routes".
	capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31}){0,3}$`)
	// A lower_snake word: kinds, roles, table names, parameter names, ids.
	wordPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	// A permission: "<verb>:<object>", for example "read:fleet" or
	// "push:node_holds"; see Permission verbs.
	permissionPattern = regexp.MustCompile(`^[a-z]+:[a-z][a-z0-9_]{0,31}(\.[a-z][a-z0-9_]{0,31}){0,2}$`)
	// A semantic version (semver 2.0.0) without a leading "v".
	semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	// A lowercase hex SHA-256.
	sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// A package member path: relative, slash-separated, every segment
	// starting with a letter, digit or underscore (so no dot segments and
	// no hidden files), at most 512 bytes.
	filePathPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`)
	// A route pattern below /api/v1/x/<id>: chi syntax with {name} parameters.
	routePatternPattern = regexp.MustCompile(`^/([a-z0-9_.-]+|\{[a-z][a-z0-9_]{0,31}\})?(/([a-z0-9_.-]+|\{[a-z][a-z0-9_]{0,31}\}))*$`)
	// A console path inside the plugin's own pages.
	consolePathPattern = regexp.MustCompile(`^/[a-z0-9_./-]{0,127}$`)
	// A link: an absolute https URL or a console path, at most maxLink bytes.
	linkPattern = regexp.MustCompile(`^(https://[^\s"<>\\]+|/[^\s"<>\\]*)$`)
	// A four-digit lowercase hex USB id.
	usbIDPattern = regexp.MustCompile(`^[0-9a-f]{4}$`)
)

// ValidFeatureKey reports whether k is a feature key. It is the same grammar
// the server's feature gate accepts.
func ValidFeatureKey(k string) bool { return featureKeyPattern.MatchString(k) }

// ValidCode reports whether c is a product code.
func ValidCode(c string) bool { return codePattern.MatchString(c) }

// ValidKeyID reports whether id is a key id.
func ValidKeyID(id string) bool { return keyIDPattern.MatchString(id) }

// ValidPackageID reports whether id is a package id.
func ValidPackageID(id string) bool { return packageIDPattern.MatchString(id) }

// ValidPublisher reports whether p is a publisher name.
func ValidPublisher(p string) bool { return publisherPattern.MatchString(p) }

// checkSortedSet reports the first element of list that breaks grammar,
// repeats or is out of order; sets on the wire are sorted so they have one
// encoding.
func checkSortedSet(path string, list []string, grammar *regexp.Regexp) *fieldError {
	for i, s := range list {
		at := joinPath(path, strconv.Itoa(i))
		if !grammar.MatchString(s) {
			return &fieldError{field: at, rule: RuleGrammar}
		}
		if i > 0 && list[i-1] == s {
			return &fieldError{field: at, rule: RuleDuplicate}
		}
		if i > 0 && list[i-1] > s {
			return &fieldError{field: at, rule: RuleOrder}
		}
	}
	return nil
}

func oneOf(path, value string, allowed ...string) *fieldError {
	if value == "" {
		return &fieldError{field: path, rule: RuleRequired}
	}
	if !slices.Contains(allowed, value) {
		return &fieldError{field: path, rule: RuleEnum}
	}
	return nil
}

// Length bounds of the grammars written without a repeat count.
const (
	maxFilePath = 512
	maxLink     = 2048
)

func matches(path, value string, grammar *regexp.Regexp) *fieldError {
	if value == "" {
		return &fieldError{field: path, rule: RuleRequired}
	}
	if !grammar.MatchString(value) || (grammar == filePathPattern && len(value) > maxFilePath) ||
		(grammar == linkPattern && len(value) > maxLink) {
		return &fieldError{field: path, rule: RuleGrammar}
	}
	return nil
}

func optionalMatches(path, value string, grammar *regexp.Regexp) *fieldError {
	if value == "" {
		return nil
	}
	return matches(path, value, grammar)
}

// text checks a required free-text field of at most maxBytes.
func text(path, s string, maxBytes int) *fieldError {
	if s == "" {
		return &fieldError{field: path, rule: RuleRequired}
	}
	if len(s) > maxBytes {
		return &fieldError{field: path, rule: RuleRange}
	}
	return nil
}

func inRange(path string, v, lo, hi int64) *fieldError {
	if v < lo || v > hi {
		return &fieldError{field: path, rule: RuleRange}
	}
	return nil
}

// first returns the first non-nil problem.
func first(problems ...*fieldError) *fieldError {
	for _, p := range problems {
		if p != nil {
			return p
		}
	}
	return nil
}

// Params are the values a client interpolates into a code's text. Keys are
// lower_snake words; values are strings of at most 512 bytes, integers or
// booleans. Numbers decode as json.Number, so integers keep their value.
type Params map[string]any

// MaxParams bounds the number of parameters of one code.
const MaxParams = 16

// UnmarshalJSON decodes numbers as json.Number.
func (p *Params) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return err
	}
	*p = m
	return nil
}

func (p Params) validate(path string) *fieldError {
	if len(p) > MaxParams {
		return &fieldError{field: path, rule: RuleRange}
	}
	for _, k := range sortedKeys(p) {
		at := joinPath(path, k)
		if !wordPattern.MatchString(k) {
			return &fieldError{field: at, rule: RuleGrammar}
		}
		switch v := p[k].(type) {
		case string:
			if len(v) > 512 {
				return &fieldError{field: at, rule: RuleRange}
			}
		case bool:
		case json.Number:
			if _, ok := safeInteger(v); !ok {
				return &fieldError{field: at, rule: RuleNumber}
			}
		case int:
			if v > maxSafeInteger || v < -maxSafeInteger {
				return &fieldError{field: at, rule: RuleNumber}
			}
		case int64:
			if v > maxSafeInteger || v < -maxSafeInteger {
				return &fieldError{field: at, rule: RuleNumber}
			}
		default:
			return &fieldError{field: at, rule: RuleType}
		}
	}
	return nil
}
