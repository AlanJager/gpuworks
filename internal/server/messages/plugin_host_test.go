package messages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every plugin host code has en and zh text, and every placeholder the
// text uses is one the code's comment names as a param.
func TestPluginHostCodesHaveClientText(t *testing.T) {
	source, err := os.ReadFile("plugin_host.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`= "(PLUGIN_[A-Z_]+)"`).FindAllSubmatch(source, -1)
	if len(declared) != len(PluginHostErrorCodes()) {
		t.Fatalf("plugin_host.go declares %d codes, PluginHostErrorCodes lists %d", len(declared), len(PluginHostErrorCodes()))
	}
	for _, locale := range []string{"en", "zh"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "ui", "src", "i18n", "locales", locale, "common.json"))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			APIError map[string]string `json:"apiError"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		for _, code := range PluginHostErrorCodes() {
			text := doc.APIError[code]
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s common.json has no apiError.%s", locale, code)
				continue
			}
			for _, m := range regexp.MustCompile(`{{(\w+)}}`).FindAllStringSubmatch(text, -1) {
				if !strings.Contains(string(source), "{"+m[1]+"}") {
					t.Errorf("%s text of %s uses {{%s}}, which plugin_host.go does not name as a param", locale, code, m[1])
				}
			}
		}
	}
}
