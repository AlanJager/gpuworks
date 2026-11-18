package hostsetup

import (
	"bytes"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

var (
	key    = extapi.HostSetupStep{Type: extapi.HostSetupUSBDevice, VendorID: "096e", ProductID: "0201"}
	second = extapi.HostSetupStep{Type: extapi.HostSetupUSBDevice, VendorID: "1050", ProductID: "0407"}
)

// The rule file of one and of two steps is the golden file byte for byte,
// in the same order whatever order the manifest lists the steps in, and
// sets group and mode only: no symlink, no RUN, no OWNER.
func TestRulesGolden(t *testing.T) {
	for _, c := range []struct {
		golden string
		steps  []extapi.HostSetupStep
	}{
		{"one_step.rules", []extapi.HostSetupStep{key}},
		{"two_steps.rules", []extapi.HostSetupStep{key, second}},
		{"two_steps.rules", []extapi.HostSetupStep{second, key}},
	} {
		got, err := Rules("acme_key", c.steps)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("testdata", c.golden))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s:\n%s\nwant:\n%s", c.golden, got, want)
		}
		for _, word := range []string{"SYMLINK", "RUN", "OWNER", "PROGRAM", "IMPORT"} {
			if bytes.Contains(got, []byte(word)) {
				t.Errorf("%s contains %s", c.golden, word)
			}
		}
	}
}

// Nothing but a package id reaches a file name, and nothing outside the
// vocabulary is rendered.
func TestRulesRefuseWhatIsNotAPackageOrAStep(t *testing.T) {
	for _, id := range []string{"", "../etc/passwd", "acme/key", "Acme_Key", "acme_key.rules", strings.Repeat("a", 65)} {
		if _, err := RuleFile(id); err == nil {
			t.Errorf("RuleFile(%q) accepted", id)
		}
		if _, err := Rules(id, []extapi.HostSetupStep{key}); err == nil {
			t.Errorf("Rules(%q) accepted", id)
		}
		if _, err := File(id); err == nil {
			t.Errorf("File(%q) accepted", id)
		}
	}
	for _, s := range []extapi.HostSetupStep{
		{Type: "shell", VendorID: "096e", ProductID: "0201"},
		{Type: extapi.HostSetupUSBDevice, VendorID: "096E", ProductID: "0201"},
		{Type: extapi.HostSetupUSBDevice, VendorID: "096e\"", ProductID: "0201"},
		{Type: extapi.HostSetupUSBDevice, VendorID: "096e", ProductID: ""},
	} {
		if _, err := Rules("acme_key", []extapi.HostSetupStep{s}); err == nil {
			t.Errorf("Rules accepted step %+v", s)
		}
		if _, err := Digest([]extapi.HostSetupStep{s}); err == nil {
			t.Errorf("Digest accepted step %+v", s)
		}
	}
	if _, err := Rules("acme_key", nil); err == nil {
		t.Error("Rules accepted no steps")
	}
	if name, _ := RuleFile("acme_key"); ruleFileID(name) != "acme_key" || ruleFileID("70-gpuworks-plugin-../x.rules") != "" || ruleFileID("99-other.rules") != "" {
		t.Fatalf("rule file names do not round-trip: %s", name)
	}
}

// The digest ignores order and duplicates and changes with any step.
func TestDigestFollowsTheSteps(t *testing.T) {
	a, _ := Digest([]extapi.HostSetupStep{key, second})
	b, _ := Digest([]extapi.HostSetupStep{second, key, key})
	c, _ := Digest([]extapi.HostSetupStep{key})
	if a != b || a == c || !a.Valid() {
		t.Fatalf("digests %s %s %s", a, b, c)
	}
}

// A package with steps gets the device directory and the read paths
// device discovery needs; one without gets nothing.
func TestLandlockOfSteps(t *testing.T) {
	if s := Landlock(nil); s.DeviceDirs != nil || s.ReadPaths != nil {
		t.Fatalf("no steps: %+v", s)
	}
	s := Landlock([]extapi.HostSetupStep{key})
	if len(s.DeviceDirs) != 1 || s.DeviceDirs[0] != "/dev" || len(s.ReadPaths) == 0 {
		t.Fatalf("one step: %+v", s)
	}
	for _, p := range append(s.DeviceDirs, s.ReadPaths...) {
		if !filepath.IsAbs(p) {
			t.Fatalf("%s is not absolute", p)
		}
	}
}

// The package imports nothing from this repository but pkg/extapi.
func TestImportsOnlyExtapi(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range parsed.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(path, "gpuworks.ai/") && path != "gpuworks.ai/gpuworks/pkg/extapi" {
				t.Errorf("%s imports %s", f, path)
			}
		}
	}
}
