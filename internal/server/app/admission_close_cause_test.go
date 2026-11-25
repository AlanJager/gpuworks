package app

import (
	"os"
	"strings"
	"testing"
)

// The documentation lists every cause the journal can name, in both
// languages, next to the log line that carries it.
func TestAdmissionCloseCausesAreDocumented(t *testing.T) {
	for _, page := range []string{"../../../docs/explanation/control-plane-cluster.md", "../../../docs-en/explanation/control-plane-cluster.md"} {
		raw, err := os.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		if !strings.Contains(text, "`closed business admission`") || !strings.Contains(text, "`caller`") {
			t.Fatalf("%s names neither the log line nor its field", page)
		}
		for cause := range admissionCloseCauses {
			if !strings.Contains(text, "`"+string(cause)+"`") {
				t.Errorf("%s does not list the close cause %s", page, cause)
			}
		}
	}
}
