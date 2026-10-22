package loadgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestClientTransportFailureDoesNotRetainURLCredentials(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close() // Exercise the real net/http transport and its *url.Error.
	base := strings.Replace(server.URL, "://", "://TRANSPORT_USER:TRANSPORT_PASSWORD@", 1) + "?api_key=TRANSPORT_QUERY&safe=yes"
	client := Client{BaseURL: base}
	result, err := client.Do(context.Background(), Request{})
	if err == nil || result.Metrics.ErrorStage != "transport" || result.Metrics.FinishedAt.IsZero() {
		t.Fatalf("transport failure lost: result=%+v err=%v", result, err)
	}
	var transportCause *net.OpError
	if !errors.As(err, &transportCause) {
		t.Fatal("transport error identity lost")
	}
	for _, text := range []string{err.Error(), NewDetailRecord(result).ErrorText} {
		assertNoCredentials(t, text, "TRANSPORT_USER", "TRANSPORT_PASSWORD", "TRANSPORT_QUERY")
		if !strings.Contains(text, "connection refused") || !strings.Contains(text, "safe=yes") {
			t.Fatalf("non-secret diagnostic lost: %s", text)
		}
	}
	_, err = client.Models(context.Background())
	if err == nil {
		t.Fatal("expected models transport failure")
	}
	assertNoCredentials(t, err.Error(), "TRANSPORT_USER", "TRANSPORT_PASSWORD", "TRANSPORT_QUERY")
}

func TestStreamErrorRedactionPreservesPartialBodyAndTiming(t *testing.T) {
	const secret = "STREAM_API_KEY_545"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"explicit body %s\"}}]}\n\n", secret)
		fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5}}\n\n")
		fmt.Fprintf(w, "data: {\"error\":{\"message\":\"rejected %s\"}}\n\n", secret)
	}))
	defer server.Close()
	result, err := (Client{BaseURL: server.URL, APIKey: secret}).Do(context.Background(), Request{Stream: true})
	if err == nil || result.Metrics.ErrorStage != "stream" || result.Metrics.Chunks != 1 || result.Metrics.InputTokens != 100 || result.Metrics.OutputTokens != 5 {
		t.Fatalf("partial stream facts lost: %+v err=%v", result, err)
	}
	detail := NewDetailRecord(result)
	assertNoCredentials(t, err.Error(), secret)
	assertNoCredentials(t, detail.ErrorText, secret)
	if detail.Text != "explicit body "+secret || detail.Metrics.FirstOutputAt == nil || detail.Metrics.LastOutputAt == nil || detail.Metrics.FinishedAt == nil || detail.Metrics.CompletionMs == nil {
		t.Fatal("explicit partial body or timing removed")
	}
}

func TestClientRedactsEncodedKnownCredentialEcho(t *testing.T) {
	const secret = `ENCODED_"API<KEY>`
	encoded, _ := json.Marshal(secret)
	for _, echo := range []string{secret, string(encoded), url.QueryEscape(secret), url.PathEscape(secret)} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, "rejected "+echo)
		}))
		_, err := (Client{BaseURL: server.URL, APIKey: secret}).Do(context.Background(), Request{})
		server.Close()
		if err == nil {
			t.Fatal("expected credential rejection")
		}
		assertNoCredentials(t, err.Error(), secret, "ENCODED_", "API", "KEY")
	}
}

func TestClientRedactsKnownAPIKeyEchoWithoutChangingRetainedBody(t *testing.T) {
	const secret = "KNOWN_API_KEY_545"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("request credentials were changed")
		}
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "rejected credential "+secret)
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, APIKey: secret}
	result, err := client.Do(context.Background(), Request{})
	if err == nil || result.Metrics.HTTPStatus != 401 || result.Metrics.ErrorStage != "http" {
		t.Fatal("HTTP failure facts lost")
	}
	assertNoCredentials(t, err.Error(), secret)
	result.Text = "explicitly retained generated text " + secret
	detail := NewDetailRecord(result)
	wire, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	assertNoCredentials(t, detail.ErrorText, secret)
	if !strings.Contains(string(wire), `"error_text":"endpoint returned 401 Unauthorized: rejected credential [REDACTED]"`) || detail.Text != result.Text {
		t.Fatalf("detail diagnostic or explicit text changed: %s", wire)
	}
	_, err = client.Models(context.Background())
	assertNoCredentials(t, err.Error(), secret)
}

func TestRedactedClientErrorsPreserveCancellationIdentity(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		var ctx context.Context
		var cancel context.CancelFunc
		want := context.Canceled
		if deadline {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		}
		result, err := (Client{BaseURL: "http://localhost:1?token=CANCEL_SECRET"}).Do(ctx, Request{})
		cancel()
		if !errors.Is(err, want) || !errors.Is(result.Metrics.Error, want) || !result.Metrics.Canceled {
			t.Fatalf("cancellation identity lost: %v", err)
		}
		assertNoCredentials(t, err.Error(), "CANCEL_SECRET")
	}
}

func TestNewDetailRecordRedactsEmbeddedURLFromExternalError(t *testing.T) {
	detail := NewDetailRecord(Result{Metrics: metrics.Request{Error: errors.New(`Get "https://USER_545:PASSWORD_545@host.test/v1?access_key=QUERY_545&safe=yes": failed`)}})
	assertNoCredentials(t, detail.ErrorText, "USER_545", "PASSWORD_545", "QUERY_545")
	if !strings.Contains(detail.ErrorText, "[diagnostic suffix omitted]") {
		t.Fatal("untrusted historical URL omission not explicit")
	}
}

func TestErrorURLCredentialKeysAndMalformedURLs(t *testing.T) {
	for _, scheme := range []string{"http", "https", "ftp", "ssh", "socks5"} {
		detail := NewDetailRecord(Result{Metrics: metrics.Request{Error: fmt.Errorf("failed %s://SCHEME_USER:SCHEME_PASSWORD@host.test/v1?token=SCHEME_QUERY", scheme)}})
		assertNoCredentials(t, detail.ErrorText, "SCHEME_USER", "SCHEME_PASSWORD", "SCHEME_QUERY")
	}
	for _, key := range []string{"token", "api_key", "api-key", "auth", "password", "passwd", "accesskey", "access_key", "private_key", "credential", "authorization", "key", "client_secret", "X-Auth-Token"} {
		for _, keySpelling := range []string{key, strings.ToUpper(key)} {
			text := NewDetailRecord(Result{Metrics: metrics.Request{Error: fmt.Errorf("failed https://user:URL_PASSWORD@host.test/v1?%s=URL_SECRET&safe=yes", keySpelling)}}).ErrorText
			assertNoCredentials(t, text, "URL_SECRET", "URL_PASSWORD", "user:")
			if !strings.Contains(text, "[diagnostic suffix omitted]") {
				t.Fatalf("unknown URL extent not explicitly omitted for %s", keySpelling)
			}
			endpoint := RedactURL(fmt.Sprintf("https://user:URL_PASSWORD@host.test/v1?%s=URL_SECRET&safe=yes", keySpelling))
			assertNoCredentials(t, endpoint, "URL_SECRET", "URL_PASSWORD", "user:")
			if !strings.Contains(endpoint, "host.test") || !strings.Contains(endpoint, "safe=yes") {
				t.Fatalf("structured endpoint facts lost for %s", keySpelling)
			}
		}
	}
	for _, raw := range []string{`parse "https://USER:BAD SECRET@host.test/v1": invalid userinfo`, `parse "https://USER:BAD%PASSWORD@host.test/v1": invalid escape`, `failed https://USER:BAD%PASSWORD@host.test/v1`} {
		text := NewDetailRecord(Result{Metrics: metrics.Request{Error: errors.New(raw)}}).ErrorText
		assertNoCredentials(t, text, "USER", "BAD", "SECRET", "PASSWORD")
		if !strings.Contains(text, "[REDACTED URL]") {
			t.Fatal("malformed URL redaction not explicit")
		}
	}
}

func TestMalformedClientURLsAndHistoricalErrorsDoNotLeakCredentialSuffixes(t *testing.T) {
	for _, password := range []string{`BAD"SECRET`, "BAD`SECRET", `BAD'"SECRET`, `BAD\"SECRET`, "BAD SECRET"} {
		base := "https://USER_545:" + password + "@host.test/v1"
		assertNoCredentials(t, RedactErrorText(base), "USER_545", "BAD", "SECRET")
		result, err := (Client{BaseURL: base}).Do(context.Background(), Request{})
		if err == nil || result.Metrics.ErrorStage != "request" {
			t.Fatalf("expected malformed request failure: %v", err)
		}
		var original *url.Error
		if !errors.As(err, &original) {
			t.Fatal("original parse error identity lost")
		}
		for _, diagnostic := range []string{err.Error(), NewDetailRecord(result).ErrorText} {
			assertNoCredentials(t, diagnostic, "USER_545", "BAD", "SECRET")
			if !strings.Contains(diagnostic, "invalid userinfo") {
				t.Fatalf("parse failure reason lost: %s", diagnostic)
			}
		}
		// Model a historical raw error written before collection-time redaction.
		_, historical := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", nil)
		if historical == nil {
			t.Fatal("expected historical parse error")
		}
		detail := NewDetailRecord(Result{Metrics: metrics.Request{Error: historical}})
		assertNoCredentials(t, detail.ErrorText, "USER_545", "BAD", "SECRET")
	}
}

func TestCompleteRawURLRedactsCredentialValueContainingSpaces(t *testing.T) {
	text := RedactURL("https://host.test/v1?authorization=Bearer SECRET_545&safe=yes")
	assertNoCredentials(t, text, "SECRET_545", "Bearer")
	if !strings.Contains(text, "host.test") || !strings.Contains(text, "safe=yes") {
		t.Fatal("safe raw URL evidence lost")
	}
}

func TestHTTPErrorWithAmbiguousUnquotedCredentialsIsRedacted(t *testing.T) {
	for _, message := range ambiguousCredentialDiagnostics() {
		t.Run(message, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, message)
			}))
			defer server.Close()
			result, err := (Client{BaseURL: server.URL, APIKey: "OTHER_CONFIGURED_KEY"}).Do(context.Background(), Request{})
			if err == nil || result.Metrics.HTTPStatus != 401 || result.Metrics.ErrorStage != "http" || result.Metrics.FinishedAt.IsZero() {
				t.Fatal("real HTTP failure facts lost")
			}
			for _, diagnostic := range []string{err.Error(), NewDetailRecord(result).ErrorText, NewDetailRecord(Result{Metrics: metrics.Request{Error: errors.New(message)}}).ErrorText} {
				assertNoCredentials(t, diagnostic, "USER_545", "BAD", "SECRET_545")
			}
		})
	}
}

func ambiguousCredentialDiagnostics() []string {
	return []string{
		"endpoint rejected https://host.test/v1?authorization=Bearer SPACE_SECRET_545&safe=yes",
		"failed https://USER_545:BAD SECRET_545@host.test/v1",
		"failed https://USER_545:BAD\nSECRET_545@host.test/v1",
		"endpoint rejected https://safe.test/?trace=yes https://USER_545:BAD_SECRET_545@host.test/v1",
		"https://safe.test/?trace=yes https://USER_545:BAD_SECRET_545@host.test/v1",
		"endpoint rejected https://safe.test/?next=https://USER_545:BAD_SECRET_545@host.test/v1",
		"endpoint rejected https://safe.test/?next=https%3A%2F%2FUSER_545%3ABAD_SECRET_545%40host.test%2Fv1",
		"error 'https://USER_545:BAD'SECRET_545@host.test/v1'",
		"error `https://USER_545:BAD`SECRET_545@host.test/v1`",
		`error "https://host.test/v1?token=BAD"SECRET_545"`,
		`error "https://USER_545:BAD" SECRET_545@host.test/v1"`,
		`error "https://USER_545:BAD": SECRET_545@host.test/v1"`,
		`error "https://host.test/v1?token=BAD": SECRET_545"`,
		`error "https://USER_545:BAD": SECRET_545@host.test/v1`,
		`error "https://host.test/v1?token=BAD": SECRET_545`,
		`first "https://safe.test/?safe=yes": failed; second https://host.test/v1?auth=Bearer SPACE_SECRET_545`,
	}
}

func assertNoCredentials(t *testing.T, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Errorf("credential %q retained in diagnostic", secret)
		}
	}
}
