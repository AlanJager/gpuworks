package messages

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func apiErrorTexts(t *testing.T, locale string) map[string]string {
	t.Helper()
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
	return doc.APIError
}

func placeholders(text string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`{{(\w+)}}`).FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// Every plugin health code has en and zh text, and every placeholder the
// text uses is one the code's comment names as a param.
func TestPluginHealthCodesHaveClientText(t *testing.T) {
	source, err := os.ReadFile("plugin_health.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`= "([A-Z_]+)"`).FindAllSubmatch(source, -1)
	if len(declared) != len(PluginHealthCodes()) {
		t.Fatalf("plugin_health.go declares %d codes, PluginHealthCodes lists %d", len(declared), len(PluginHealthCodes()))
	}
	for _, locale := range []string{"en", "zh"} {
		texts := apiErrorTexts(t, locale)
		for _, code := range PluginHealthCodes() {
			text := texts[code]
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s common.json has no apiError.%s", locale, code)
				continue
			}
			for _, p := range placeholders(text) {
				if !strings.Contains(string(source), "{"+p+"}") {
					t.Errorf("%s text of %s uses {{%s}}, which plugin_health.go does not name as a param", locale, code, p)
				}
			}
		}
	}
}

// The banners reuse PLUGIN_CRASHLOOP and PLUGIN_INCOMPATIBLE: each text
// names the plugin, so a banner above every page says which one, and uses
// only params every site sets.
func TestReusedBannerCodesNameThePlugin(t *testing.T) {
	sets := map[string][]string{
		ErrorPluginCrashloop:    {"id", "crashes", "window_s", "retry_s"},
		ErrorPluginIncompatible: {"id", "capability", "version", "offered", "min_core_hint"},
	}
	for _, locale := range []string{"en", "zh"} {
		texts := apiErrorTexts(t, locale)
		for code, params := range sets {
			used := placeholders(texts[code])
			if !strings.Contains(texts[code], "{{id}}") {
				t.Errorf("%s text of %s = %q does not name the plugin", locale, code, texts[code])
			}
			for _, p := range used {
				if !slices.Contains(params, p) {
					t.Errorf("%s text of %s uses {{%s}}, which not every site sets", locale, code, p)
				}
			}
		}
	}
}
