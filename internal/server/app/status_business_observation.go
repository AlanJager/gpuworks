//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func probeBusiness(ctx context.Context, httpAddr string, timeout time.Duration) string {
	classification, _ := observeBusiness(ctx, httpAddr, timeout)
	return classification
}

// Observation diagnostics use fixed text. Transport errors can contain URLs,
// and response bodies are untrusted; neither belongs in the status report.
func observeBusiness(ctx context.Context, httpAddr string, timeout time.Duration) (string, string) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodGet, "http://"+httpAddr+"/api/v1/readyz", nil)
	if err != nil {
		return "not observed", "readyz request could not be constructed"
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: timeout}}
	response, err := client.Do(request)
	if err != nil {
		return "not observed", businessObservationError("request", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 513))
	if err != nil {
		return "not observed", businessObservationError("response body", err)
	}
	if len(body) > 512 {
		return "not observed", "readyz response body exceeds 512 bytes"
	}
	text := strings.Join(strings.Fields(string(body)), " ")
	if strings.Contains(text, "member business service is not ready") {
		return "member business service is not ready", ""
	}
	switch response.StatusCode {
	case http.StatusOK:
		if response.Header.Get("Content-Type") == "application/json" && response.Header.Get("Cache-Control") == "no-store" && bytes.Equal(bytes.TrimSpace(body), []byte(`{"status":"ready"}`)) {
			return "open", ""
		}
		return "invalid readiness response", "readyz response does not match the readiness contract"
	default:
		return fmt.Sprintf("HTTP %d", response.StatusCode), ""
	}
}

func businessObservationError(phase string, err error) string {
	if errors.Is(err, context.Canceled) {
		return "readyz " + phase + " canceled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "readyz " + phase + " timed out"
	}
	if phase == "response body" {
		return "readyz response body read failed"
	}
	return "readyz request transport failed"
}
