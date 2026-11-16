package extapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// maxSafeInteger is the largest integer every JSON reader represents
// exactly (2^53-1). Every number on the wire stays within ±maxSafeInteger.
const maxSafeInteger = 1<<53 - 1

// maxDepth bounds nesting so a hostile document cannot exhaust the stack.
const maxDepth = 64

// parseTree reads data as exactly one JSON value into a tree of
// map[string]any, []any, string, json.Number, bool and nil. It refuses
// invalid UTF-8, a \u escape of a lone UTF-16 surrogate, duplicate keys,
// non-integer or unsafe numbers, nesting deeper than maxDepth and trailing
// data.
func parseTree(data []byte) (any, *fieldError) {
	if !utf8.Valid(data) || loneSurrogate(data) {
		return nil, &fieldError{rule: RuleSyntax}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, ferr := parseValue(dec, "", 0)
	if ferr != nil {
		return nil, ferr
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &fieldError{rule: RuleTrailing}
	}
	return v, nil
}

// loneSurrogate reports whether data has a \u escape of a UTF-16 surrogate
// that is not half of a high-low pair. encoding/json would decode it as
// U+FFFD, so two different documents would decode, and canonicalize, to the
// same value. A backslash appears only inside strings in valid JSON; in
// anything else the decoder fails anyway.
func loneSurrogate(data []byte) bool {
	for i := 0; i < len(data); i++ {
		if data[i] != '\\' {
			continue
		}
		r, ok := uEscape(data, i)
		if !ok {
			i++ // skip the escaped character, which may be a backslash
			continue
		}
		i += 5
		switch {
		case r >= 0xdc00 && r <= 0xdfff:
			return true
		case r >= 0xd800 && r <= 0xdbff:
			low, ok := uEscape(data, i+1)
			if !ok || low < 0xdc00 || low > 0xdfff {
				return true
			}
			i += 6
		}
	}
	return false
}

// uEscape decodes the \uXXXX escape at data[i:], if there is one.
func uEscape(data []byte, i int) (rune, bool) {
	if i+6 > len(data) || data[i] != '\\' || data[i+1] != 'u' {
		return 0, false
	}
	n, err := strconv.ParseUint(string(data[i+2:i+6]), 16, 16)
	return rune(n), err == nil
}

func parseValue(dec *json.Decoder, path string, depth int) (any, *fieldError) {
	if depth > maxDepth {
		return nil, &fieldError{field: path, rule: RuleRange}
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, &fieldError{field: path, rule: RuleSyntax}
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := map[string]any{}
			for dec.More() {
				ktok, err := dec.Token()
				if err != nil {
					return nil, &fieldError{field: path, rule: RuleSyntax}
				}
				key, _ := ktok.(string)
				child := joinPath(path, key)
				if _, dup := obj[key]; dup {
					return nil, &fieldError{field: child, rule: RuleDuplicate}
				}
				v, ferr := parseValue(dec, child, depth+1)
				if ferr != nil {
					return nil, ferr
				}
				obj[key] = v
			}
			if _, err := dec.Token(); err != nil {
				return nil, &fieldError{field: path, rule: RuleSyntax}
			}
			return obj, nil
		case '[':
			arr := []any{}
			for i := 0; dec.More(); i++ {
				v, ferr := parseValue(dec, joinPath(path, strconv.Itoa(i)), depth+1)
				if ferr != nil {
					return nil, ferr
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, &fieldError{field: path, rule: RuleSyntax}
			}
			return arr, nil
		}
		return nil, &fieldError{field: path, rule: RuleSyntax}
	case json.Number:
		if _, ok := safeInteger(t); !ok {
			return nil, &fieldError{field: path, rule: RuleNumber}
		}
		return t, nil
	default:
		return t, nil
	}
}

// safeInteger parses n as an integer within ±maxSafeInteger.
func safeInteger(n json.Number) (int64, bool) {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		return 0, false
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil || i > maxSafeInteger || i < -maxSafeInteger {
		return 0, false
	}
	return i, true
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// decodeStrict decodes data into v, a pointer to one of this package's wire
// types, refusing anything a strict reader must refuse: what parseTree
// refuses, any object key that is not exactly a field of the target type,
// fixed-length arrays of another length, and values of the wrong type.
// Display objects are the only part decoded leniently.
func decodeStrict(object string, data []byte, v any) error {
	tree, ferr := parseTree(data)
	if ferr != nil {
		return malformed(object, ferr)
	}
	if ferr := checkShape(reflect.TypeOf(v).Elem(), tree, ""); ferr != nil {
		if ferr.rule == "" {
			return newError(ErrorWireUnknownField, map[string]any{"object": object, "field": ferr.field},
				"%s: unknown field %s", object, ferr.field)
		}
		return malformed(object, ferr)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		field := ""
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			field = typeErr.Field
		}
		return malformed(object, &fieldError{field: field, rule: RuleType})
	}
	return nil
}

func malformed(object string, f *fieldError) *Error {
	return newError(ErrorWireMalformed,
		map[string]any{"object": object, "field": f.field, "rule": f.rule},
		"%s: malformed at %q (%s)", object, f.field, f.rule)
}

var (
	timeType       = reflect.TypeFor[Time]()
	displayType    = reflect.TypeFor[Display]()
	rawMessageType = reflect.TypeFor[json.RawMessage]()
	paramsType     = reflect.TypeFor[Params]()
)

// checkShape walks tree against t. It reports an unknown key as a
// fieldError with an empty rule, and values of the wrong JSON type with
// RuleType, so the decoder never matches a key case-insensitively, never
// drops an array element and never reaches a custom decoder with a value it
// would refuse without saying where.
func checkShape(t reflect.Type, v any, path string) *fieldError {
	if v == nil {
		return nil // null leaves the zero value; Validate decides if it may.
	}
	switch t {
	case timeType:
		s, ok := v.(string)
		if !ok {
			return &fieldError{field: path, rule: RuleType}
		}
		if _, err := parseTime(s); err != nil {
			return &fieldError{field: path, rule: RuleGrammar}
		}
		return nil
	case displayType, paramsType:
		if _, ok := v.(map[string]any); !ok {
			return &fieldError{field: path, rule: RuleType}
		}
		return nil // lenient inside; Validate bounds it.
	case rawMessageType:
		return nil
	}
	switch t.Kind() {
	case reflect.String:
		if _, ok := v.(string); !ok {
			return &fieldError{field: path, rule: RuleType}
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			return &fieldError{field: path, rule: RuleType}
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, ok := v.(json.Number)
		if !ok {
			return &fieldError{field: path, rule: RuleType}
		}
		if i, _ := safeInteger(n); i < 0 && t.Kind() >= reflect.Uint {
			return &fieldError{field: path, rule: RuleNumber}
		}
	case reflect.Pointer:
		return checkShape(t.Elem(), v, path)
	case reflect.Struct:
		obj, ok := v.(map[string]any)
		if !ok {
			return &fieldError{field: path, rule: RuleType}
		}
		fields := fieldsOf(t)
		for _, key := range sortedKeys(obj) {
			idx, ok := fields[key]
			if !ok {
				return &fieldError{field: joinPath(path, key)}
			}
			if ferr := checkShape(t.Field(idx).Type, obj[key], joinPath(path, key)); ferr != nil {
				return ferr
			}
		}
	case reflect.Map:
		obj, ok := v.(map[string]any)
		if !ok {
			return &fieldError{field: path, rule: RuleType}
		}
		for _, key := range sortedKeys(obj) {
			if ferr := checkShape(t.Elem(), obj[key], joinPath(path, key)); ferr != nil {
				return ferr
			}
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			s, ok := v.(string)
			if !ok {
				return &fieldError{field: path, rule: RuleType}
			}
			if _, err := base64.StdEncoding.DecodeString(s); err != nil {
				return &fieldError{field: path, rule: RuleGrammar}
			}
			return nil
		}
		arr, ok := v.([]any)
		if !ok {
			return &fieldError{field: path, rule: RuleType}
		}
		for i, el := range arr {
			if ferr := checkShape(t.Elem(), el, joinPath(path, strconv.Itoa(i))); ferr != nil {
				return ferr
			}
		}
	}
	return nil
}

var fieldCache sync.Map // reflect.Type -> map[string]int

// fieldsOf maps each JSON name of struct type t to its field index.
func fieldsOf(t reflect.Type) map[string]int {
	if m, ok := fieldCache.Load(t); ok {
		return m.(map[string]int)
	}
	m := map[string]int{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !f.IsExported() || name == "-" || name == "" {
			continue
		}
		m[name] = i
	}
	fieldCache.Store(t, m)
	return m
}

// Display is display-only data: labels, descriptions, links an older reader
// may drop. It is the one lenient part of a wire object: any JSON object of
// at most MaxDisplayBytes, kept as received so a relay loses nothing. It
// never changes what the server enforces.
type Display map[string]json.RawMessage

// MaxDisplayBytes bounds one Display object, encoded.
const MaxDisplayBytes = 8 << 10

func (d Display) validate(path string) *fieldError {
	if d == nil {
		return nil
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return &fieldError{field: path, rule: RuleType}
	}
	if len(raw) > MaxDisplayBytes {
		return &fieldError{field: path, rule: RuleRange}
	}
	return nil
}
