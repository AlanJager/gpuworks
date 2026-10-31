package batch

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Cancellable is what Cancel does: true exactly for the statuses Cancel moves
// to cancelling. A cancelling batch is answered as it is; every other status
// is refused with batch_not_cancellable, finalizing included.
func TestCancellableIsWhatCancelDoes(t *testing.T) {
	statuses := []string{StatusValidating, StatusInProgress, StatusFinalizing, StatusCancelling, StatusCompleted, StatusFailed, StatusExpired, StatusCancelled}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			r := newRig(t, Config{}, echo)
			now := r.clock.Now()
			if err := r.repo.CreateBatch(Batch{ID: "batch_" + status, UserID: testOwner.UserID, Status: status, Endpoint: EndpointChat, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			got, err := r.svc.Cancel(testOwner, "batch_"+status)
			moved := err == nil && got.Status == StatusCancelling && got.CancellingAt != nil && status != StatusCancelling
			if moved != Cancellable(status) {
				t.Fatalf("Cancellable(%s) = %v, but Cancel = %+v, %v", status, Cancellable(status), got, err)
			}
			if status == StatusCancelling {
				if err != nil || got.Status != StatusCancelling {
					t.Fatalf("a cancelling batch = %+v, %v", got, err)
				}
				return
			}
			var e *Error
			if !Cancellable(status) && (!errors.As(err, &e) || e.Status != 409 || e.Code != CodeBatchNotCancellable || !strings.Contains(e.Message, status)) {
				t.Fatalf("cancel of a %s batch = %v", status, err)
			}
		})
	}
}

// The output and error files are kept the configured retention after the batch
// ended; a running batch, or one that wrote neither file, has no such time.
func TestOutputExpiresAtFollowsTheRetention(t *testing.T) {
	r := newRig(t, Config{OutputRetention: 7 * 24 * time.Hour}, echo)
	ended := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		b    Batch
		want *time.Time
	}{
		{"running", Batch{Status: StatusInProgress}, nil},
		{"failed without files", Batch{Status: StatusFailed, FailedAt: &ended}, nil},
		{"completed with output", Batch{Status: StatusCompleted, OutputFileID: "o", CompletedAt: &ended}, ptr(ended.Add(7 * 24 * time.Hour))},
		{"cancelled with errors", Batch{Status: StatusCancelled, ErrorFileID: "e", CancelledAt: &ended}, ptr(ended.Add(7 * 24 * time.Hour))},
		{"expired with both", Batch{Status: StatusExpired, OutputFileID: "o", ErrorFileID: "e", ExpiredAt: &ended}, ptr(ended.Add(7 * 24 * time.Hour))},
	} {
		got := r.svc.OutputExpiresAt(tc.b)
		if (got == nil) != (tc.want == nil) || (got != nil && !got.Equal(*tc.want)) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	// The default retention is 30 days.
	d := newRig(t, Config{}, echo)
	if got := d.svc.OutputExpiresAt(Batch{OutputFileID: "o", CompletedAt: &ended}); got == nil || !got.Equal(ended.Add(30*24*time.Hour)) {
		t.Fatalf("default retention: %v", got)
	}
}

// The refusal of a window names the accepted range from the constants.
func TestWindowRefusalNamesTheRange(t *testing.T) {
	r := newRig(t, Config{}, echo)
	f, err := r.svc.Upload(testOwner, "in.jsonl", strings.NewReader(inputLines(1)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.svc.CreateBatch(testOwner, CreateRequest{InputFileID: f.ID, Endpoint: EndpointChat, CompletionWindow: "100h"})
	var e *Error
	if !errors.As(err, &e) || e.Message != `completion_window must be a whole number of hours from 1h to 72h, such as "24h"` {
		t.Fatalf("err = %v", err)
	}
}

func TestNamedModels(t *testing.T) {
	for _, tc := range []struct {
		in    []string
		named string
		more  int
		text  string
	}{
		{nil, "", 0, ""},
		{[]string{"a", "b"}, "a|b", 0, "a, b"},
		{[]string{"a", "b", "c", "d", "e"}, "a|b|c|d|e", 0, "a, b, c, d, e"},
		{[]string{"a", "b", "c", "d", "e", "f", "g"}, "a|b|c|d|e", 2, "a, b, c, d, e, ..."},
	} {
		named, more := NamedModels(tc.in)
		if strings.Join(named, "|") != tc.named || more != tc.more || ModelsText(tc.in) != tc.text {
			t.Errorf("%v: %v %d %q", tc.in, named, more, ModelsText(tc.in))
		}
	}
}

func ptr[T any](v T) *T { return &v }
