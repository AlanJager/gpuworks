package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func priorityRequest(t *testing.T, contentType, body string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "http://gw/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	r.Header.Set("X-Vllm-Priority", "-100")
	return r
}

func forwardedBody(t *testing.T, r *http.Request) string {
	t.Helper()
	body, err := r.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A client's priority never reaches an engine: the header goes, and the
// field becomes the neutral 0 (vLLM rejects null) while the rest of the body
// stays as sent.
func TestStripClientPriority(t *testing.T) {
	r := priorityRequest(t, "application/json", `{"model":"m","priority":-100,"messages":[{"role":"user","content":"say \"priority\""}],"stream":true}`)
	cleanup, err := stripClientPriority(r, DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil {
		t.Fatal(err)
	}
	if r.Header.Get("X-Vllm-Priority") != "" {
		t.Fatal("the priority header was forwarded")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(forwardedBody(t, r)), &got); err != nil {
		t.Fatal(err)
	}
	if got["priority"] != float64(0) || got["model"] != "m" || got["stream"] != true {
		t.Fatalf("body = %v", got)
	}
	messages, _ := got["messages"].([]any)
	if first, _ := messages[0].(map[string]any); first["content"] != `say "priority"` {
		t.Fatalf("message content changed: %v", messages)
	}
}

func TestStripClientPriorityLeavesOtherBodiesAlone(t *testing.T) {
	// A body that does not name the field is not copied.
	plain := `{"model":"m","messages":[]}`
	r := priorityRequest(t, "application/json", plain)
	cleanup, err := stripClientPriority(r, DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil || forwardedBody(t, r) != plain || r.Header.Get("X-Vllm-Priority") != "" {
		t.Fatalf("err=%v body=%q header=%q", err, forwardedBody(t, r), r.Header.Get("X-Vllm-Priority"))
	}
	// The word inside a string is not the field.
	nested := `{"model":"m","messages":[{"role":"user","content":"\"priority\": 5"}]}`
	r = priorityRequest(t, "application/json", nested)
	cleanup, err = stripClientPriority(r, DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil || forwardedBody(t, r) != nested {
		t.Fatalf("err=%v body=%q", err, forwardedBody(t, r))
	}
	// A multipart upload is not JSON; only the header goes.
	form := "--b\r\nContent-Disposition: form-data; name=\"priority\"\r\n\r\n5\r\n--b--\r\n"
	r = priorityRequest(t, "multipart/form-data; boundary=b", form)
	cleanup, err = stripClientPriority(r, DefaultMaxBodyBytes)
	defer cleanup()
	if err != nil || forwardedBody(t, r) != form || r.Header.Get("X-Vllm-Priority") != "" {
		t.Fatalf("err=%v body=%q", err, forwardedBody(t, r))
	}
}

// A key spelled with an escaped letter is the plain key to the engine's
// parser, so the scan reports every key as named and the caller looks them
// up exactly; the escapes clients commonly send do not. Matches across two
// reads are found, a byte at a time.
func TestBodyNamesSeesEscapedKeys(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []bool
	}{
		{"plain", `{"model":"m","priority":1}`, []bool{false, true}},
		{"escaped priority", jsonEscapes(`{"model":"m","%u0070riority":1}`), []bool{true, true}},
		{"escaped kv field", jsonEscapes(`{"kv_tr%u0061nsfer_params":{}}`), []bool{true, true}},
		{"escaped underscore", jsonEscapes(`{"kv%u005ftransfer_params":{}}`), []bool{true, true}},
		{"non-ASCII text", jsonEscapes(`{"messages":[{"content":"%u4e2d%u6587 caf%u00e9"}]}`), []bool{false, false}},
		{"HTML-safe escapes", jsonEscapes(`{"messages":[{"content":"%u003cb%u003e %u0026"}]}`), []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, oneByte := range []bool{false, true} {
				var source io.Reader = strings.NewReader(tc.body)
				if oneByte {
					source = iotest.OneByteReader(source)
				}
				found := make([]bool, 2)
				if err := readerContainsEach(source, [][]byte{[]byte(`"` + kvTransferField + `"`), []byte(`"priority"`)}, found); err != nil {
					t.Fatal(err)
				}
				if found[0] != tc.want[0] || found[1] != tc.want[1] {
					t.Fatalf("one byte at a time %v: found = %v, want %v", oneByte, found, tc.want)
				}
			}
		})
	}
}
