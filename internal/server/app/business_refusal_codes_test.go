package app

import (
	"os"
	"strings"
	"testing"
)

// The documentation lists every code the header can carry, in both languages.
func TestRefusalCodesAreDocumented(t *testing.T) {
	for _, page := range []string{"../../../docs/explanation/control-plane-cluster.md", "../../../docs-en/explanation/control-plane-cluster.md"} {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !strings.Contains(text, businessRefusalHeader) || !strings.Contains(text, "`business_refusals`") {
			t.Fatalf("%s names neither the header nor the health field", page)
		}
		for code := range businessRefusalCodes {
			if !strings.Contains(text, "`"+code+"`") {
				t.Errorf("%s does not list the refusal code %s", page, code)
			}
		}
	}
}
