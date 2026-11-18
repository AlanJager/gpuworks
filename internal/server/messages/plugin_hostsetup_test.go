package messages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every host setup code has en and zh text, and the text interpolates only
// the params its call sites set: {id} and, for the required step, the
// {command} to run.
func TestPluginHostSetupCodesHaveClientText(t *testing.T) {
	params := map[string][]string{
		ErrorPluginHostSetupRequired:    {"id", "command"},
		ErrorPluginHostSetupUnsupported: {"id"},
	}
	if len(params) != len(PluginHostSetupErrorCodes()) {
		t.Fatalf("PluginHostSetupErrorCodes lists %v", PluginHostSetupErrorCodes())
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
		for _, code := range PluginHostSetupErrorCodes() {
			text := doc.APIError[code]
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s common.json has no apiError.%s", locale, code)
				continue
			}
			for _, m := range regexp.MustCompile(`{{(\w+)}}`).FindAllStringSubmatch(text, -1) {
				if !strings.Contains(strings.Join(params[code], ","), m[1]) {
					t.Errorf("%s text of %s uses {{%s}}, which its call sites do not set", locale, code, m[1])
				}
			}
		}
	}
}
