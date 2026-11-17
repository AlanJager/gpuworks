package serverconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type reportsSection struct {
	Key     string   `yaml:"key"`
	Servers []string `yaml:"servers"`
	Retries int      `yaml:"retries"`
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(content), 0o664); err != nil {
		t.Fatal(err)
	}
	return path
}

const withExtensions = `listen:
  console: [127.0.0.1:8800]
extensions:
  reports:
    key: /etc/gpuworks/reports.txt
    servers: [a, b]
  billing:
    currency: CNY
`

func TestLoadKeepsExtensionSectionsAndSectionDecodesThem(t *testing.T) {
	f, err := Load(writeConfig(t, withExtensions))
	if err != nil {
		t.Fatal(err)
	}
	if got := UnknownSections(f, []string{"reports"}); !reflect.DeepEqual(got, []string{"billing"}) {
		t.Fatalf("unknown sections = %q, want [billing]", got)
	}
	if got := UnknownSections(f, nil); !reflect.DeepEqual(got, []string{"billing", "reports"}) {
		t.Fatalf("unknown sections without modules = %q, want both, sorted", got)
	}
	v := reportsSection{Retries: 3}
	if err := f.Section("reports", &v); err != nil {
		t.Fatal(err)
	}
	want := reportsSection{Key: "/etc/gpuworks/reports.txt", Servers: []string{"a", "b"}, Retries: 3}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("section = %+v, want %+v (an absent key keeps its default)", v, want)
	}
}

// A file written before the extensions key existed parses as before.
func TestLoadWithoutExtensionsIsUnchanged(t *testing.T) {
	f, err := Load(writeConfig(t, "listen:\n  console: [127.0.0.1:8800]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Extensions != nil || len(UnknownSections(f, nil)) != 0 {
		t.Fatalf("extensions = %+v, want none", f.Extensions)
	}
	v := reportsSection{Key: "default"}
	if err := f.Section("reports", &v); err != nil || v.Key != "default" {
		t.Fatalf("absent section changed v: %+v, %v", v, err)
	}
}

// Only extensions is raw: a misspelt top-level key still stops the load.
func TestLoadStillRejectsAnUnknownTopLevelKey(t *testing.T) {
	_, err := Load(writeConfig(t, withExtensions+"extension:\n  reports: {}\n"))
	if err == nil || !strings.Contains(err.Error(), "extension") {
		t.Fatalf("err = %v, want the unknown key named", err)
	}
}

func TestSectionDecodesStrictly(t *testing.T) {
	f, err := Load(writeConfig(t, withExtensions+"  quota:\n    nodes: 4\n    node: 5\n  bad: [1, 2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var quota struct {
		Nodes int `yaml:"nodes"`
	}
	if err := f.Section("quota", &quota); err == nil || !strings.Contains(err.Error(), "extensions.quota") || !strings.Contains(err.Error(), "node") {
		t.Fatalf("err = %v, want the unknown key of extensions.quota", err)
	}
	var bad reportsSection
	if err := f.Section("bad", &bad); err == nil || !strings.Contains(err.Error(), "extensions.bad") {
		t.Fatalf("err = %v, want a list refused for a mapping", err)
	}
}

// Write (what install uses) keeps the sections, and Load reads them back.
func TestRenderRoundTripKeepsExtensionSections(t *testing.T) {
	f, err := Load(writeConfig(t, withExtensions))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), FileName)
	if err := Write(path, f, 0o664); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var v reportsSection
	if err := back.Section("reports", &v); err != nil || v.Key != "/etc/gpuworks/reports.txt" || len(v.Servers) != 2 {
		t.Fatalf("section after round trip = %+v, %v", v, err)
	}
	var billing struct {
		Currency string `yaml:"currency"`
	}
	if err := back.Section("billing", &billing); err != nil || billing.Currency != "CNY" {
		t.Fatalf("billing after round trip = %+v, %v", billing, err)
	}
	// The server's single-key edits keep them too.
	if err := SetUsageReport(path, false); err != nil {
		t.Fatal(err)
	}
	back, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := UnknownSections(back, nil); !reflect.DeepEqual(got, []string{"billing", "reports"}) {
		t.Fatalf("sections after an edit = %q", got)
	}
}
