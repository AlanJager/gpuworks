package app

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"strings"
	"testing"
)

// A handler that reports progress with 102 must still be able to send its
// final status: the material manifest receiver answers 204 after such
// responses, and its caller rejects the default 200.
func TestAccessLogHandlerForwardsInformationalResponsesBeforeTheFinalStatus(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))
	server := httptest.NewServer(accessLogHandler(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusProcessing)
		w.WriteHeader(http.StatusProcessing)
		w.Header().Set("X-Final", "yes")
		w.WriteHeader(http.StatusNoContent)
	})))
	defer server.Close()

	informational := 0
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		if code == http.StatusProcessing {
			informational++
		}
		return nil
	}}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/progress", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request.WithContext(httptrace.WithClientTrace(request.Context(), trace)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.Header.Get("X-Final") != "yes" {
		t.Fatalf("final response = %d X-Final=%q, want 204 with the final header", response.StatusCode, response.Header.Get("X-Final"))
	}
	if informational != 2 {
		t.Fatalf("client saw %d informational responses, want 2", informational)
	}
	if record := logged.String(); !strings.Contains(record, "status=204") || strings.Contains(record, "status=102") {
		t.Fatalf("access log records %q, want the final status 204", record)
	}
}

func TestAccessLogHandlerTreatsAProtocolSwitchAsFinal(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &accessResponseWriter{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusSwitchingProtocols)
	writer.WriteHeader(http.StatusOK)
	if writer.status != http.StatusSwitchingProtocols || !writer.written {
		t.Fatalf("status = %d written = %v, want 101 recorded as final", writer.status, writer.written)
	}
}
