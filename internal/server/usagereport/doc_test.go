package usagereport

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docPath is the public list of every field the report carries; it is the
// program's promise to its users, so it and Report must name the same fields.
const docPath = "../../../docs/reference/usage-report.md"

func TestDocumentedFieldsAreExactlyTheReportFields(t *testing.T) {
	raw, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]bool{}
	row := regexp.MustCompile("^\\| (`[^|]+`) \\|")
	field := regexp.MustCompile("`([a-z_.]+)`")
	for _, line := range strings.Split(string(raw), "\n") {
		m := row.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, f := range field.FindAllStringSubmatch(m[1], -1) {
			documented[f[1]] = true
		}
	}
	actual := map[string]bool{}
	leaves(reflect.TypeOf(Report{}), "", actual)
	var missing, extra []string
	for f := range actual {
		if !documented[f] {
			missing = append(missing, f)
		}
	}
	for f := range documented {
		if !actual[f] {
			extra = append(extra, f)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("%s and usagereport.Report disagree:\n  sent but not documented: %v\n  documented but not sent: %v", docPath, missing, extra)
	}
}

// leaves collects the JSON paths of a report's leaf fields; a distribution
// (Counts) is a leaf.
func leaves(t reflect.Type, prefix string, out map[string]bool) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		path := prefix + name
		if f.Type.Kind() == reflect.Struct {
			leaves(f.Type, path+".", out)
			continue
		}
		out[path] = true
	}
}
