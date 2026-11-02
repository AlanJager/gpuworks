package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type trackedErrorBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *trackedErrorBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}
func (b *trackedErrorBody) Close() error { b.closed = true; return nil }

func TestBoundUpstreamErrorFramingAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name, raw, encoding string
		length              int64
	}{
		{name: "chunked", raw: strings.Repeat("A", 6_492_508), length: -1},
		{name: "declared huge", raw: strings.Repeat("A", 6_492_508), length: 6_492_508},
		{name: "compressed", raw: "compressed binary", encoding: "gzip", length: 17},
		{name: "pydantic", raw: `{"detail":[{"msg":"Audio is unsupported","input":{"data":"PRIVATE"},"ctx":{"input":"PRIVATE"}}]}`, length: -1},
		{name: "plaintext", raw: "Validation failed input_value={'data':'PRIVATE'}", length: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &trackedErrorBody{reader: strings.NewReader(tc.raw)}
			resp := &http.Response{StatusCode: 400, Body: body, ContentLength: tc.length, Header: make(http.Header), TransferEncoding: []string{"chunked"}}
			resp.Header.Set("Content-Encoding", tc.encoding)
			resp.Header.Set("Content-Type", "application/json")
			if err := boundUpstreamError(resp); err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > 4096 || bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, bytes.Repeat([]byte("A"), 128)) {
				t.Fatalf("unsafe error (%d bytes): %.128s", len(raw), raw)
			}
			if body.read > maxUpstreamErrorBytes+1 || !body.closed {
				t.Fatalf("read=%d closed=%v", body.read, body.closed)
			}
			if resp.ContentLength != int64(len(raw)) || resp.Header.Get("Content-Encoding") != "" || len(resp.TransferEncoding) != 0 {
				t.Fatalf("incorrect replacement framing: %+v", resp)
			}
		})
	}
}

func TestBoundUpstreamErrorPreservesCompactErrorsAndSuccess(t *testing.T) {
	for _, raw := range []string{`{"error":{"message":"Rate limited","code":"rate_limit","type":"rate_limit_error"}}`, `{"detail":"Not Found"}`} {
		resp := &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader(raw)), ContentLength: int64(len(raw)), Header: make(http.Header)}
		if err := boundUpstreamError(resp); err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		if string(got) != raw {
			t.Fatalf("compact wire changed: %s", got)
		}
	}
	body := &trackedErrorBody{reader: strings.NewReader("data: {\"choices\":[]}\n\n")}
	resp := &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}
	if err := boundUpstreamError(resp); err != nil || resp.Body != body || body.closed || body.read != 0 {
		t.Fatalf("successful stream changed: err=%v body=%+v", err, body)
	}
}
