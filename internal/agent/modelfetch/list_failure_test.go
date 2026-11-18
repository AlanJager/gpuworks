package modelfetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

func TestListingFinalFailureClassification(t *testing.T) {
	status := func(code int) error { return fmt.Errorf("provider: %w", &modelrepo.StatusError{Code: code}) }
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"not found", status(404), proto.ErrorModelDownloadSourceNotFound},
		{"gone", status(410), proto.ErrorModelDownloadSourceNotFound},
		{"unauthorized", status(401), proto.ErrorModelDownloadSourceAccessDenied},
		{"forbidden", status(403), proto.ErrorModelDownloadSourceAccessDenied},
		{"throttled", status(429), proto.ErrorModelDownloadSourceThrottled},
		{"unavailable", status(503), proto.ErrorModelDownloadNetworkExhausted},
		{"bad request", status(400), proto.ErrorModelDownloadSourceRejected},
		{"deadline", fmt.Errorf("listing: %w", context.DeadlineExceeded), proto.ErrorModelDownloadNetworkExhausted},
		{"temporary DNS", &net.DNSError{Err: "try later", IsTemporary: true}, proto.ErrorModelDownloadNetworkExhausted},
		{"same causes", errors.Join(status(404), status(410)), proto.ErrorModelDownloadSourceNotFound},
		{"mixed wrapped causes", fmt.Errorf("all sources: %w", errors.Join(status(404), context.DeadlineExceeded)), ""},
		{"mixed authentication", errors.Join(status(403), status(500)), ""},
		{"cancel", context.Canceled, ""},
		{"mixed cancel", errors.Join(context.Canceled, context.DeadlineExceeded), ""},
		{"unknown status", status(418), ""},
		{"local failure", os.ErrPermission, ""},
		{"untyped message", errors.New("HTTP 404: context deadline exceeded"), ""},
		{"nil", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ListingErrorCode(tc.err); got != tc.want {
				t.Fatalf("code=%q want=%q error=%v", got, tc.want, tc.err)
			}
		})
	}
}

func TestListingMixedSourcesDoNotInventOneTaskCause(t *testing.T) {
	missing, busy := newHub(t, modelFiles()), newHub(t, modelFiles())
	missing.listStatus, busy.listStatus = 404, 502
	f := newFetcher(t, nil, &run{}, missing.URL, busy.URL)
	_, err := f.List(context.Background(), commit)
	if err == nil || ListingErrorCode(err) != "" {
		t.Fatalf("mixed exhausted sources were misclassified: %v", err)
	}
}
