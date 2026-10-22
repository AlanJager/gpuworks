package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

type cloudTransportFunc func(*http.Request) (*http.Response, error)

func (f cloudTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func clientForTest(t *testing.T, options ...ClientOption) *APIClient {
	t.Helper()
	c, err := New(provider.Credentials{
		Endpoint: "https://cloud.example/zstack", PublicKey: "test-access-key", PrivateKey: "test-access-secret",
	}, options...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func cloudResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func clientError(t *testing.T, err error, kind ErrorKind) *APIError {
	t.Helper()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != kind {
		t.Fatalf("error = %v, want Cloud error kind %s", err, kind)
	}
	return apiErr
}

func TestCloudClientRejectsInvalidPolicy(t *testing.T) {
	for name, option := range map[string]ClientOption{
		"nil": nil, "request": WithRequestTimeout(0), "operation": WithOperationTimeout(-time.Second),
		"poll": WithPollInterval(0), "retries": WithReadRetries(-1),
		"initial backoff": WithRetryBackoff(0, time.Second),
		"backoff bounds":  WithRetryBackoff(time.Second, time.Millisecond),
		"server timeout":  WithServerAPITimeout(-time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(provider.Credentials{Endpoint: "https://cloud.example", PublicKey: "id", PrivateKey: "secret"}, option); err == nil {
				t.Fatal("client accepted an invalid API policy")
			}
			c := clientForTest(t)
			c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
				t.Fatal("invalid call policy reached the network")
				return nil, nil
			})
			var out any
			if err := c.Get(t.Context(), "/hosts", &out, option); err == nil {
				t.Fatal("query accepted an invalid override")
			}
			if err := c.PutAction(t.Context(), "/actions", map[string]any{}, option); err == nil {
				t.Fatal("mutation accepted an invalid override")
			}
		})
	}
}

func TestCloudClientRejectsInvalidCredentials(t *testing.T) {
	for _, creds := range []provider.Credentials{
		{Endpoint: "https://user:secret@cloud.example", PublicKey: "id", PrivateKey: "secret"},
		{Endpoint: "file:///tmp/cloud", PublicKey: "id", PrivateKey: "secret"},
		{Endpoint: "https://cloud.example?secret=value", PublicKey: "id", PrivateKey: "secret"},
		{Endpoint: "https://cloud.example/#fragment", PublicKey: "id", PrivateKey: "secret"},
		{Endpoint: "https://cloud.example", PrivateKey: "secret"},
		{Endpoint: "https://cloud.example", PublicKey: "id"},
	} {
		if _, err := New(creds); err == nil {
			t.Fatal("client accepted invalid endpoint or missing credentials")
		}
	}
}

func TestCloudQueryErrorsPreserveCodesWithoutRetryingBusinessFailures(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		kind   ErrorKind
		code   string
	}{
		{"authentication", 401, `{"error":{"code":"AUTH.1000","details":"test-access-secret"}}`, ErrorAuthentication, "AUTH.1000"},
		{"permission", 403, `{"error":{"code":"AUTH.1001"}}`, ErrorAuthentication, "AUTH.1001"},
		{"business failure", 503, `{"error":{"code":"SYS.1001"}}`, ErrorOperation, "SYS.1001"},
		{"error in success envelope", 200, `{"error":{"code":"SYS.1002"}}`, ErrorOperation, "SYS.1002"},
		{"malformed error envelope", 503, `{"error":"proxy failure"}`, ErrorResponse, ""},
		{"missing resource", 404, `{"diagnostic":{"code":"20007"}}`, ErrorRequest, "20007"},
		{"bad JSON", 200, `{"secret":"test-access-secret"`, ErrorResponse, ""},
		{"empty body", 200, ``, ErrorResponse, ""},
		{"null body", 200, `null`, ErrorResponse, ""},
		{"array body", 200, `[]`, ErrorResponse, ""},
		{"trailing response", 200, `{} {}`, ErrorResponse, ""},
		{"unexpected async query", 202, `{"location":"/v1/api-jobs/unexpected"}`, ErrorResponse, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := clientForTest(t, WithReadRetries(3))
			calls := 0
			c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return cloudResponse(r, test.status, test.body), nil
			})
			var out any
			err := c.Get(t.Context(), "/hosts", &out)
			apiErr := clientError(t, err, test.kind)
			if calls != 1 || apiErr.StatusCode != test.status || apiErr.Code != test.code || apiErr.OutcomeUnknown {
				t.Fatalf("query error lost semantics: calls=%d error=%+v", calls, apiErr)
			}
			if strings.Contains(err.Error(), "test-access-secret") {
				t.Fatal("remote description leaked through error text")
			}
		})
	}
}

func TestCloudQueryPreservesDiagnosticAlongsideAPIError(t *testing.T) {
	c := clientForTest(t)
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		return cloudResponse(r, 503, `{"diagnostic":{"code":"20007","message":"test-access-secret"},"error":{"code":"SYS.1007"}}`), nil
	})
	var out any
	err := c.Get(t.Context(), "/resource-configurations/host/kvm/reservedCpu", &out)
	apiErr := clientError(t, err, ErrorOperation)
	if apiErr.Code != "SYS.1007" || apiErr.DiagnosticCode != "20007" || strings.Contains(err.Error(), "test-access-secret") {
		t.Fatalf("error codes were lost or diagnostic message leaked: code=%q diagnostic=%q error=%v", apiErr.Code, apiErr.DiagnosticCode, err)
	}
}

func TestCloudQueryReturnsInventoryAndClosesResponse(t *testing.T) {
	c := clientForTest(t)
	closed := false
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		if !validTestAccessKey(r) || r.URL.Path != "/zstack/v1/hosts" || r.URL.Query().Get("q") != "name=test host" {
			t.Fatal("query lost its endpoint, query or signature")
		}
		response := cloudResponse(r, 200, "")
		response.Body = &cloudTestBody{Reader: strings.NewReader(`{"inventories":[{"uuid":"host"}],"error":null}`), close: func() { closed = true }}
		return response, nil
	})
	var out struct {
		Inventories []struct {
			UUID string `json:"uuid"`
		} `json:"inventories"`
	}
	if err := c.Get(t.Context(), "/hosts?q=name=test%20host", &out); err != nil {
		t.Fatal(err)
	}
	if !closed || len(out.Inventories) != 1 || out.Inventories[0].UUID != "host" {
		t.Fatalf("query result=%+v bodyClosed=%v", out, closed)
	}
}

type cloudTestBody struct {
	io.Reader
	close func()
}

func (b *cloudTestBody) Close() error { b.close(); return nil }

func TestCloudClientBoundsResponseAndClosesRejectedBody(t *testing.T) {
	c := clientForTest(t, WithReadRetries(2))
	closed, calls := false, 0
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		response := cloudResponse(r, 200, "")
		response.Body = &cloudTestBody{Reader: strings.NewReader(`{"value":"` + strings.Repeat("x", maxCloudResponseBytes) + `"}`), close: func() { closed = true }}
		return response, nil
	})
	var out any
	clientError(t, c.Get(t.Context(), "/hosts", &out), ErrorResponse)
	if !closed || calls != 1 {
		t.Fatalf("oversized response was leaked or retried: closed=%v calls=%d", closed, calls)
	}
}

func TestCloudClientLogsAttemptsWithoutCredentialOrBodyContent(t *testing.T) {
	var log bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := clientForTest(t, WithClientLogger(logger))
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		return cloudResponse(r, 503, `{"error":{"code":"SYS.1000","description":"test-access-secret","details":"private remote detail"}}`), nil
	})
	var out any
	err := c.Get(t.Context(), "/hosts?q=name=test-access-secret", &out)
	clientError(t, err, ErrorOperation)
	var record map[string]any
	if err := json.Unmarshal(log.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["method"] != "GET" || record["path"] != "/hosts" || record["attempt"] != float64(1) || record["status"] != float64(503) || record["duration"] == nil {
		t.Fatalf("missing request diagnostics: %v", record)
	}
	for _, private := range []string{"test-access-key", "test-access-secret", "Authorization", "private remote detail"} {
		if strings.Contains(log.String()+err.Error(), private) {
			t.Fatalf("request diagnostics leaked %s", private)
		}
	}
}

func TestCloudClientRejectsBadPathAndPayloadWithoutSending(t *testing.T) {
	c := clientForTest(t)
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid request reached transport")
		return nil, nil
	})
	for _, path := range []string{"https://elsewhere.example/hosts", "//elsewhere.example/hosts", "/hosts#fragment", "/hosts%zz"} {
		var out any
		clientError(t, c.Get(t.Context(), path, &out), ErrorRequest)
	}
	clientError(t, c.PutAction(context.Background(), "/actions", make(chan int)), ErrorRequest)
}
