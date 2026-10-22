package aihelper

import (
	"strings"
	"testing"
)

func TestWithResponseLanguage(t *testing.T) {
	forced := WithResponseLanguage("system", "zh-CN", false)
	if !strings.HasSuffix(forced, "Write everything the user sees in Simplified Chinese, including any note before a tool call.") || strings.Contains(forced, "different language") {
		t.Fatalf("forced prompt = %q", forced)
	}
	free := WithResponseLanguage("system", "fr", true)
	if !strings.Contains(free, "Write everything the user sees in English, including any note before a tool call.") || !strings.Contains(free, "If the user writes in a different language") {
		t.Fatalf("free-conversation prompt = %q", free)
	}
}
