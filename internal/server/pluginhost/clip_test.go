package pluginhost

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// An overlong English fallback is cut at a rune boundary: the refusal stays
// valid UTF-8 within the bound and decodes on the other side.
func TestAnOverlongMessageIsCutAtARuneBoundary(t *testing.T) {
	x := &extapi.Error{Code: extapi.ErrorWireMalformed, Message: "a" + strings.Repeat("é", extapi.MaxErrorMessage),
		Params: map[string]any{"object": "request", "field": "_root", "rule": extapi.RuleSyntax}}
	w := wireRefusal(x).Wire
	if len(w.Message) > extapi.MaxErrorMessage || len(w.Message) < extapi.MaxErrorMessage-1 || !utf8.ValidString(w.Message) {
		t.Fatalf("message of %d bytes, valid UTF-8 %v", len(w.Message), utf8.ValidString(w.Message))
	}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extapi.DecodeWireError(raw); err != nil {
		t.Fatalf("the clipped refusal does not decode: %v", err)
	}
}
