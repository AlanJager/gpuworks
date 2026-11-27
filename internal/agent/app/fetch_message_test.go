package app

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

type sentStatuses struct {
	mu   sync.Mutex
	list []proto.ModelFileStatus
}

func (s *sentStatuses) send(st proto.ModelFileStatus) {
	s.mu.Lock()
	s.list = append(s.list, st)
	s.mu.Unlock()
}

func (s *sentStatuses) last() proto.ModelFileStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.list) == 0 {
		return proto.ModelFileStatus{}
	}
	return s.list[len(s.list)-1]
}

func newTestFetchMessage() (*fetchMessage, *sentStatuses) {
	sent := &sentStatuses{}
	base := proto.ModelFileStatus{Key: "k", State: proto.MFDownloading, Path: "/models/m", Acquisition: proto.ModelAcquireDownload}
	return newFetchMessage(base, sent.send), sent
}

func throttle(retryAfter time.Duration) error {
	return &download.StatusError{Code: http.StatusTooManyRequests, Detail: "download: unexpected probe status 429", RetryAfter: retryAfter}
}

// While the source has asked the download to wait, the status says so, at
// once and with the progress already reached; bytes beyond that point put the
// address back.
func TestTheStatusSaysWhenTheSourceAskedTheDownloadToWait(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.progress(proto.ModelFileStatus{Key: "k", State: proto.MFDownloading, BytesDone: 40 << 30, BytesTotal: 60 << 30, RateBytesPerS: 1 << 20, Revision: "abc"})
	m.retrying(throttle(110*time.Second), 1, 16, 110*time.Second)

	want := "downloading from huggingface.co (rate limited by the source; retrying in 1m50s)"
	got := sent.last()
	if got.Message != want {
		t.Fatalf("message %q, want %q", got.Message, want)
	}
	if got.BytesDone != 40<<30 || got.BytesTotal != 60<<30 || got.Revision != "abc" || got.RateBytesPerS != 0 {
		t.Fatalf("the wait was announced as %+v: the progress reached must stay, with no rate", got)
	}

	// A resumed file reports the bytes already on disk first: not new bytes.
	m.progress(proto.ModelFileStatus{BytesDone: 40 << 30, BytesTotal: 60 << 30})
	if sent.last().Message != want {
		t.Fatalf("a report of the bytes already there ended the wait: %q", sent.last().Message)
	}
	m.progress(proto.ModelFileStatus{BytesDone: 40<<30 + 4096, BytesTotal: 60 << 30})
	if sent.last().Message != downloadingFrom("huggingface.co") {
		t.Fatalf("new bytes left %q", sent.last().Message)
	}
}

// Other retries (a reset, a server error) are not waits the source asked
// for, so the message does not say rate limited; the retry itself is still
// reported, as a field: a download that keeps failing near its end must not
// read as one that stopped (GWKS-526).
func TestOtherRetriesAreReportedAsARetryNotAsRateLimited(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("hf-mirror.com"))
	m.at(14_598_320_397, 14_606_988_015, "abc")
	m.retrying(fmt.Errorf("download tokenizer.json: %w", errors.New("read tcp 10.0.0.2:60286->39.99.133.195:443: read: connection reset by peer")), 3, 16, 8*time.Second)
	got := sent.last()
	if got.Message != downloadingFrom("hf-mirror.com") {
		t.Fatalf("message %q: a reset is not the source asking to wait", got.Message)
	}
	want := proto.ModelFetchRetry{Attempt: 4, Attempts: 16, WaitSeconds: 8, Cause: "read tcp 10.0.0.2:60286->39.99.133.195:443: read: connection reset by peer"}
	if got.Retry == nil || *got.Retry != want {
		t.Fatalf("retry %+v, want %+v", got.Retry, want)
	}
	if got.BytesDone != 14_598_320_397 || got.BytesTotal != 14_606_988_015 || got.RateBytesPerS != 0 {
		t.Fatalf("the retry was announced as %+v: the bytes reached must stay, with no rate", got)
	}
	m.retrying(&download.StatusError{Code: http.StatusInternalServerError, Detail: "500"}, 4, 16, 15*time.Second)
	if r := sent.last().Retry; r == nil || r.Attempt != 5 || r.Throttled || r.Cause == "" {
		t.Fatalf("a server error's retry is %+v", r)
	}
}

// A throttled answer is a retry too, marked as the source's wait and without
// a cause: the message already says it is rate limited.
func TestAThrottledRetryIsMarkedAsTheSourcesWait(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.retrying(throttle(110*time.Second), 1, 16, 110*time.Second)
	want := proto.ModelFetchRetry{Attempt: 2, Attempts: 16, WaitSeconds: 110, Throttled: true}
	if r := sent.last().Retry; r == nil || *r != want {
		t.Fatalf("retry %+v, want %+v", r, want)
	}
}

// The retry ends when the retried file moves: the bytes it was announced at
// are not new ones. Moving to another address ends it too.
func TestNewBytesOrASwitchEndTheRetry(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("modelscope.cn"))
	m.at(100, 200, "abc")
	m.retrying(errors.New("connection reset by peer"), 1, 16, time.Second)
	m.progress(proto.ModelFileStatus{BytesDone: 100, BytesTotal: 200})
	if sent.last().Retry == nil {
		t.Fatal("restating the bytes already there ended the retry")
	}
	m.progress(proto.ModelFileStatus{BytesDone: 120, BytesTotal: 200, RateBytesPerS: 4096})
	if sent.last().Retry != nil {
		t.Fatalf("new bytes left the retry %+v", sent.last().Retry)
	}
	m.retrying(errors.New("connection reset by peer"), 1, 6, time.Second)
	m.set(downloadingFrom("hf-mirror.com") + " (switched: modelscope.cn failed 6 times)")
	if sent.last().Retry != nil {
		t.Fatal("the retry of the address before outlived the switch")
	}
}

// The cause is the innermost failure, cut on a character boundary.
func TestARetryCauseIsTheInnermostFailureBounded(t *testing.T) {
	if got := retryCause(fmt.Errorf("download a: %w", fmt.Errorf("get b: %w", errors.New("read: connection reset by peer")))); got != "read: connection reset by peer" {
		t.Fatalf("cause %q", got)
	}
	cause := retryCause(errors.New(strings.Repeat("失", 200)))
	if !utf8.ValidString(cause) || len(cause) > retryCauseLimit+len("…") {
		t.Fatalf("cause is %d bytes, valid=%v", len(cause), utf8.ValidString(cause))
	}
}

// A throttled listing (a modelrepo error, not a download one) is a wait too.
func TestAThrottledListingSaysRateLimited(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.retrying(&modelrepo.StatusError{Code: http.StatusTooManyRequests, RetryAfter: 30 * time.Second}, 2, 16, 30*time.Second)
	if !strings.Contains(sent.last().Message, "rate limited by the source; retrying in 30s") {
		t.Fatalf("message %q", sent.last().Message)
	}
}

// Moving to another address ends the wait the last address asked for, and the
// switch is announced with the progress reached.
func TestASwitchEndsTheWaitOfTheAddressBefore(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("hf-mirror.com"))
	m.at(7<<20, 9<<20, "abc")
	m.retrying(throttle(5*time.Second), 1, 2, 5*time.Second)
	m.set(downloadingFrom("huggingface.co") + " (switched: hf-mirror.com failed 6 times)")
	got := sent.last()
	if strings.Contains(got.Message, "rate limited") || !strings.HasPrefix(got.Message, "downloading from huggingface.co") {
		t.Fatalf("message %q", got.Message)
	}
	if got.BytesDone != 7<<20 || got.BytesTotal != 9<<20 {
		t.Fatalf("the switch was announced as %+v", got)
	}
}
