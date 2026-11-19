package messages

import (
	"slices"
	"strings"
	"testing"
)

// Every plugin lifecycle code has en and zh text that interpolates only
// the params every call site sets.
func TestPluginAdminCodesHaveClientText(t *testing.T) {
	params := map[string][]string{
		ErrorPluginDisabled: {"id"},
		ErrorPluginUninstallConfirmRequired: {"id", "confirm", "held_nodes", "users_affected", "caps_lifted", "features_locked",
			"banners", "overrides"},
		ErrorPluginUpgradeRolledBack: {"id", "version", "previous", "cause_code"},
		ErrorPluginSnapshotFailed:    {"id"},
		ErrorPluginNoPreviousVersion: {"id"},
		ErrorPluginSnapshotMissing:   {"id", "version"},
		ErrorPluginUpgradeInProgress: {"id"},
		ErrorPluginUpgradeIDMismatch: {"id", "package_id"},
		ErrorPluginUpgradeCancelled:  {"id"},
	}
	if len(params) != len(PluginAdminCodes()) {
		t.Fatalf("PluginAdminCodes lists %v", PluginAdminCodes())
	}
	for _, locale := range []string{"en", "zh"} {
		texts := apiErrorTexts(t, locale)
		for _, code := range PluginAdminCodes() {
			text := texts[code]
			if strings.TrimSpace(text) == "" {
				t.Errorf("%s common.json has no apiError.%s", locale, code)
				continue
			}
			if !strings.Contains(text, "{{id}}") {
				t.Errorf("%s text of %s = %q does not name the plugin", locale, code, text)
			}
			for _, p := range placeholders(text) {
				if !slices.Contains(params[code], p) {
					t.Errorf("%s text of %s uses {{%s}}, which its call sites do not set", locale, code, p)
				}
			}
		}
	}
}
