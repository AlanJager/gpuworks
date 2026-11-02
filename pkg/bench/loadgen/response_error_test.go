package loadgen

import (
	"strings"
	"testing"
)

func TestResponseErrorTextRedactsAfterJSONDecoding(t *testing.T) {
	for _, wire := range []string{
		`{"error":{"message":"engine failed https:\/\/url-user:url-password@host.test\/?access_token=url-query"}}`,
		`{"error":"engine failed \u0068ttps\u003a\u002f\u002furl-user:url-password@host.test/?access_token=url-query"}`,
		`{"error":{"message":"engine failed with a\u0070i-secret"}}`,
	} {
		text := ResponseErrorText("endpoint returned 400 Bad Request: "+wire, "api-secret")
		for _, secret := range []string{"url-user", "url-password", "url-query", "api-secret"} {
			if strings.Contains(text, secret) {
				t.Fatalf("decoded diagnostic retained credential: %q", text)
			}
		}
		if !strings.Contains(text, "engine failed") {
			t.Fatalf("useful cause lost: %q", text)
		}
	}
}
