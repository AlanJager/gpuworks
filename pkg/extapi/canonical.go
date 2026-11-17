package extapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"
)

// Canonical returns data in canonical JSON (see the package documentation).
// It fails on anything parseTree refuses.
func Canonical(data []byte) ([]byte, error) {
	tree, ferr := parseTree(data)
	if ferr != nil {
		return nil, malformed("json", ferr)
	}
	var buf bytes.Buffer
	writeCanonical(&buf, tree)
	return buf.Bytes(), nil
}

// IsCanonical reports whether data is already in canonical form.
func IsCanonical(data []byte) bool {
	c, err := Canonical(data)
	return err == nil && bytes.Equal(c, data)
}

// Marshal encodes v, one of this package's wire types, in canonical form.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return Canonical(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func writeCanonical(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		i, _ := safeInteger(t) // parseTree admitted only safe integers.
		buf.WriteString(strconv.FormatInt(i, 10))
	case string:
		writeCanonicalString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, el := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonical(buf, el)
		}
		buf.WriteByte(']')
	case map[string]any:
		buf.WriteByte('{')
		for i, k := range sortedKeys(t) {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCanonicalString(buf, k)
			buf.WriteByte(':')
			writeCanonical(buf, t[k])
		}
		buf.WriteByte('}')
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Go compares strings bytewise: UTF-8 byte order.
	return keys
}

const hexDigits = "0123456789abcdef"

func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hexDigits[r>>4])
				buf.WriteByte(hexDigits[r&0xf])
				continue
			}
			var b [utf8.UTFMax]byte
			buf.Write(b[:utf8.EncodeRune(b[:], r)])
		}
	}
	buf.WriteByte('"')
}

// Digest names one document: "sha256:" and the lowercase hex SHA-256 of its
// canonical bytes.
type Digest string

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Valid reports whether d has the digest grammar.
func (d Digest) Valid() bool { return digestPattern.MatchString(string(d)) }

// DigestOf returns the digest of canonical bytes.
func DigestOf(canonical []byte) Digest {
	sum := sha256.Sum256(canonical)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// DigestValue returns the digest of v's canonical encoding. Both sides of a
// push compute it the same way, so a plugin can skip a push whose digest the
// server already holds.
func DigestValue(v any) (Digest, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return DigestOf(b), nil
}

// Time is a wire timestamp: RFC 3339 in UTC with whole seconds, for example
// "2026-01-02T03:04:05Z". Other spellings of the same instant are refused,
// so a timestamp has one encoding.
type Time struct{ time.Time }

const timeLayout = "2006-01-02T15:04:05Z"

// NewTime returns t in UTC, truncated to the second.
func NewTime(t time.Time) Time { return Time{t.UTC().Truncate(time.Second)} }

var errTimeEncoding = errors.New("time is not UTC with whole seconds")

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, err
	}
	if t.Format(timeLayout) != s {
		return time.Time{}, errTimeEncoding
	}
	return t, nil
}

// MarshalJSON encodes t; it fails on a time that is not UTC with whole
// seconds rather than change it silently.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.Location() != time.UTC || t.Nanosecond() != 0 {
		return nil, errTimeEncoding
	}
	return []byte(`"` + t.Format(timeLayout) + `"`), nil
}

// UnmarshalJSON decodes the one accepted spelling.
func (t *Time) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := parseTime(s)
	if err != nil {
		return err
	}
	t.Time = parsed
	return nil
}
