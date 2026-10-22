package aihelper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/itchyny/gojq"
)

// What one call_api answer shows at most: arrays are cut to the limit,
// strings to maxStringChars, and the whole answer to maxAnswerChars, each
// cut saying so and how to narrow the read instead.
const (
	defaultListLimit = 20
	maxListLimit     = 200
	maxStringChars   = 300
	maxAnswerChars   = 12000
	jqTimeout        = 2 * time.Second
)

// now is the clock times are aged against; tests replace it.
var now = time.Now

// sensitiveKey matches field names whose string values never reach the
// model: tokens, secrets, passwords, keys and credentials (a node's agent
// token is one of them; "prompt_tokens" and "has_ssh_credential" are not -
// the first is a count, the second not a string). Environment variable names
// such as AWS_SECRET_ACCESS_KEY match the same way.
var sensitiveKey = regexp.MustCompile(`(?i)(^|_)(token|secret|password|passwd|passphrase|api_?key|access_?key|private_?key|credential|authorization|cookie)s?$`)

// sensitiveFlag matches a command-line flag whose value is a secret:
// "--api-key", "--hf-token", "-password"; "--tokenizer" and
// "--max-num-batched-tokens" (a count) stay readable.
var sensitiveFlag = regexp.MustCompile(`(?i)^--?[a-z0-9_-]*(token|secret|password|passwd|api[_-]?key|access[_-]?key|private[_-]?key|credential)s?$`)

// Secrets written inside a string: a URL's userinfo password, a flag's value
// ("--api-key sk-..", "--hf-token=hf_.."), an assignment in a command,
// mount options or an env dump ("password=..", "HF_TOKEN=.."), and a
// bearer token.
var (
	urlPassword  = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+):[^@\s/]+@`)
	inlineFlag   = regexp.MustCompile(`(?i)(--?[a-z0-9_-]*(?:token|secret|password|passwd|api[_-]?key|access[_-]?key|private[_-]?key|credential)s?)(=|\s+)([^\s"',]+)`)
	inlineAssign = regexp.MustCompile(`(?i)((?:^|[\s,;"'])[a-z0-9_]*(?:token|secret|password|passwd|passphrase|api_?key|access_?key|private_?key|credential)s?)=([^\s,;"']+)`)
	bearer       = regexp.MustCompile(`(?i)(bearer\s+)[a-z0-9._~+/=-]{8,}`)
	numeric      = regexp.MustCompile(`^[0-9.]+$`)
)

const redacted = "[redacted]"

// redact replaces every secret the answer carries: the string value of a
// sensitive field, the value of a {name, value} pair whose name is
// sensitive, the argument after a sensitive flag in an argv array, and the
// secrets written inside strings.
func redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if s, ok := x.(string); ok && s != "" && sensitiveKey.MatchString(k) {
				t[k] = redacted
				continue
			}
			t[k] = redact(x)
		}
		if name, ok := pairName(t); ok && sensitiveKey.MatchString(name) {
			if s, ok := t["value"].(string); ok && s != "" {
				t["value"] = redacted
			}
		}
	case []any:
		for i := range t {
			if i > 0 {
				if flag, ok := t[i-1].(string); ok && sensitiveFlag.MatchString(flag) {
					if s, ok := t[i].(string); ok && s != "" && !numeric.MatchString(s) {
						t[i] = redacted
						continue
					}
				}
			}
			t[i] = redact(t[i])
		}
	case string:
		return redactString(t)
	}
	return v
}

// pairName is the name of an environment-style {name|key, value} pair.
func pairName(m map[string]any) (string, bool) {
	if _, ok := m["value"]; !ok {
		return "", false
	}
	for _, k := range []string{"name", "key"} {
		if s, ok := m[k].(string); ok {
			return s, true
		}
	}
	return "", false
}

func redactString(s string) string {
	if len(s) < 8 {
		return s
	}
	s = urlPassword.ReplaceAllString(s, "$1:"+redacted+"@")
	s = bearer.ReplaceAllString(s, "${1}"+redacted)
	s = inlineFlag.ReplaceAllStringFunc(s, func(m string) string {
		parts := inlineFlag.FindStringSubmatch(m)
		if numeric.MatchString(parts[3]) || parts[3] == redacted {
			return m
		}
		return parts[1] + parts[2] + redacted
	})
	return inlineAssign.ReplaceAllStringFunc(s, func(m string) string {
		parts := inlineAssign.FindStringSubmatch(m)
		if numeric.MatchString(parts[2]) || parts[2] == redacted {
			return m
		}
		return parts[1] + "=" + redacted
	})
}

// timeKey matches the fields a time is written in: RFC 3339 strings named
// *_at and Unix milliseconds named *_ms or time_ms.
var timeKey = regexp.MustCompile(`(^|_)at$|(^|_)ms$`)

// humanize writes each time as UTC with its age, "2026-10-10T03:14:59Z (29.2h
// ago)", so the model need not do date arithmetic it gets wrong. Durations
// named *_ms (small numbers) are left alone.
func humanize(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if !timeKey.MatchString(k) {
				t[k] = humanize(x)
				continue
			}
			switch val := x.(type) {
			case float64:
				if val > 1e12 { // a Unix millisecond time, not a duration
					t[k] = aged(time.UnixMilli(int64(val)))
				}
			case string:
				if at, err := time.Parse(time.RFC3339Nano, val); err == nil {
					t[k] = aged(at)
				}
			}
		}
	case []any:
		for i := range t {
			t[i] = humanize(t[i])
		}
	}
	return v
}

func aged(at time.Time) string {
	d := now().Sub(at)
	var age string
	switch {
	case d < 0:
		age = "in the future"
	case d < time.Minute:
		age = fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		age = fmt.Sprintf("%.0fm ago", d.Minutes())
	case d < 72*time.Hour:
		age = fmt.Sprintf("%.1fh ago", d.Hours())
	default:
		age = fmt.Sprintf("%.1fd ago", d.Hours()/24)
	}
	return at.UTC().Format(time.RFC3339) + " (" + age + ")"
}

// shape cuts arrays to limit and long strings, saying how much was cut.
func shape(v any, limit int) any {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			t[k] = shape(x, limit)
		}
		return t
	case []any:
		if len(t) <= limit {
			for i := range t {
				t[i] = shape(t[i], limit)
			}
			return t
		}
		out := make([]any, 0, limit+1)
		for _, x := range t[:limit] {
			out = append(out, shape(x, limit))
		}
		return append(out, fmt.Sprintf("…%d more items (raise limit, page with the endpoint's own parameters, or filter with jq)", len(t)-limit))
	case string:
		if r := []rune(t); len(r) > maxStringChars {
			return string(r[:maxStringChars]) + fmt.Sprintf("…[%d more chars]", len(r)-maxStringChars)
		}
	}
	return v
}

// outline is a value's structure without its data: keys and value types,
// two levels of objects deep and one element of each array.
func outline(v any, depth int) any {
	switch t := v.(type) {
	case map[string]any:
		if depth >= 3 {
			return "{…}"
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := map[string]any{}
		for i, k := range keys {
			if i == 40 {
				out["…"] = fmt.Sprintf("%d more keys", len(keys)-40)
				break
			}
			out[k] = outline(t[k], depth+1)
		}
		return out
	case []any:
		if len(t) == 0 {
			return []any{}
		}
		return []any{outline(t[0], depth+1), fmt.Sprintf("×%d", len(t))}
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

func outlineText(v any) string {
	b, _ := json.Marshal(outline(v, 0))
	return clip(string(b), 3000)
}

// runJQ applies a jq filter. Several results come back as an array.
func runJQ(ctx context.Context, filter string, input any) (any, error) {
	q, err := gojq.Parse(filter)
	if err != nil {
		return nil, err
	}
	code, err := gojq.Compile(q)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, jqTimeout)
	defer cancel()
	var out []any
	iter := code.RunWithContext(ctx, input)
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := v.(error); isErr {
			var halt *gojq.HaltError
			if errors.As(err, &halt) && halt.Value() == nil {
				break
			}
			return nil, err
		}
		out = append(out, v)
		if len(out) > 10000 {
			return nil, errors.New("jq produced more than 10000 results; aggregate or select first")
		}
	}
	if len(out) == 1 {
		return out[0], nil
	}
	if out == nil {
		out = []any{}
	}
	return out, nil
}

// isEmptyAnswer reports an answer that holds nothing: null, [], {}, or a
// collection envelope with no items.
func isEmptyAnswer(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		if len(t) == 0 {
			return true
		}
		if items, ok := t["items"].([]any); ok && len(items) == 0 {
			return true
		}
	}
	return false
}

// compact renders a value for the model, bounded by maxAnswerChars including
// the truncation note: the runner cuts any tool result past
// maxToolResultChars, which would drop the note and its hint.
func compact(v any, hint string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if len(s) <= maxAnswerChars {
		return s
	}
	note := func(shown int) string {
		return fmt.Sprintf("\n[truncated: %d of %d chars shown. %s]", shown, len(s), hint)
	}
	// The note is measured with the largest count it can show, so the real
	// one is never longer.
	head := clipToFit(s, maxAnswerChars-len(note(maxAnswerChars)))
	return head + note(len(strings.TrimSuffix(head, "…")))
}

// clipToFit is clip whose result, marker included, is at most n bytes.
func clipToFit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return clip(s, max(n-len("…"), 0))
}

// jqField matches the object keys a jq filter names: .name, ?.name, .name?
var jqField = regexp.MustCompile(`\.([A-Za-z_][A-Za-z0-9_]*)`)

// jqShorthand matches a key written alone in an object constructor,
// {name, other: ...}, which jq reads as name: .name.
var jqShorthand = regexp.MustCompile(`[{,]\s*([A-Za-z_][A-Za-z0-9_]*)\s*[,}]`)

var jqKeywords = map[string]bool{"if": true, "then": true, "elif": true, "else": true, "end": true, "and": true, "or": true, "not": true,
	"as": true, "def": true, "reduce": true, "foreach": true, "try": true, "catch": true, "label": true, "null": true, "true": true, "false": true}

// jqFieldNames are the object keys a jq filter reads.
func jqFieldNames(filter string) []string {
	var out []string
	for _, m := range jqField.FindAllStringSubmatch(filter, -1) {
		out = append(out, m[1])
	}
	// Shorthand keys share their delimiters ("{a, b}"), so scan on from each
	// key rather than from the end of the previous match.
	for at := 0; at < len(filter); {
		loc := jqShorthand.FindStringSubmatchIndex(filter[at:])
		if loc == nil {
			break
		}
		if name := filter[at+loc[2] : at+loc[3]]; !jqKeywords[name] {
			out = append(out, name)
		}
		at += loc[3]
	}
	return out
}

// fieldHints names the fields a failed jq filter used that the answer does
// not have anywhere, each with the closest one it does have, so the model
// fixes the filter from one sentence instead of the whole outline.
func fieldHints(filter string, data any) string {
	have := map[string]bool{}
	collectKeys(data, have, 0)
	var hints []string
	seen := map[string]bool{}
	for _, name := range jqFieldNames(filter) {
		if have[name] || seen[name] {
			continue
		}
		seen[name] = true
		if near := closestKey(name, have); near != "" {
			hints = append(hints, fmt.Sprintf("%q (did you mean %q?)", name, near))
		} else {
			hints = append(hints, fmt.Sprintf("%q", name))
		}
	}
	if len(hints) == 0 {
		return ""
	}
	return " The answer has no field " + strings.Join(hints, ", ") + "."
}

func collectKeys(v any, into map[string]bool, depth int) {
	if depth > 6 {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			into[k] = true
			collectKeys(x, into, depth+1)
		}
	case []any:
		for i, x := range t {
			if i == 50 {
				break
			}
			collectKeys(x, into, depth+1)
		}
	}
}

// closestKey is the key sharing most "_"-separated words with name, the
// shorter on a tie; "" when none shares a word.
func closestKey(name string, have map[string]bool) string {
	words := map[string]bool{}
	for _, w := range strings.Split(name, "_") {
		words[w] = true
	}
	best, bestCommon := "", 0
	for k := range have {
		common := 0
		for _, w := range strings.Split(k, "_") {
			if words[w] {
				common++
			}
		}
		if common > bestCommon || (common == bestCommon && common > 0 && (len(k) < len(best) || (len(k) == len(best) && k < best))) {
			best, bestCommon = k, common
		}
	}
	return best
}
