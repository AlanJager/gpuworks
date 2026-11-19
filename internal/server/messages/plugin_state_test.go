package messages

import (
	"slices"
	"testing"
)

// The offline maintenance banner has en and zh text that interpolates
// exactly the params every call site sets.
func TestPluginMaintenanceFailedHasClientText(t *testing.T) {
	for _, locale := range []string{"en", "zh"} {
		text := apiErrorTexts(t, locale)[ErrorPluginMaintenanceFailed]
		got := placeholders(text)
		slices.Sort(got)
		if !slices.Equal(got, []string{"code", "op"}) {
			t.Errorf("%s text of %s = %q; want it to use {{op}} and {{code}}", locale, ErrorPluginMaintenanceFailed, text)
		}
	}
}
