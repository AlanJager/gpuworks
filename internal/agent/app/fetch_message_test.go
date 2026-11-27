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

	want := "downloading from huggingface.co (rate limited by the source; retrying in 1m50s, attempt 2 of 16)"
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

// Other retries (a reset, a server error) are not waits the source asked for,
// but they are announced too: a download that keeps failing near its start
// would otherwise look like one that has finished the bytes it reached
// (GWKS-526).
func TestOtherRetriesSayWhichAttemptComesNextAndWhy(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("hf-mirror.com"))
	m.at(14_598_320_397, 14_606_988_015, "abc")
	m.retrying(fmt.Errorf("download tokenizer.json: %w", errors.New("read tcp 10.0.0.2:60286->39.99.133.195:443: read: connection reset by peer")), 3, 16, 8*time.Second)
	got := sent.last()
	want := "downloading from hf-mirror.com (retrying in 8s, attempt 4 of 16 after: read tcp 10.0.0.2:60286->39.99.133.195:443: read: connection reset by peer)"
	if got.Message != want {
		t.Fatalf("message %q, want %q", got.Message, want)
	}
	if strings.Contains(got.Message, "rate limited") {
		t.Fatalf("a reset read as the source asking to wait: %q", got.Message)
	}
	if got.BytesDone != 14_598_320_397 || got.BytesTotal != 14_606_988_015 || got.RateBytesPerS != 0 {
		t.Fatalf("the retry was announced as %+v: the bytes reached must stay, with no rate", got)
	}
	m.retrying(&download.StatusError{Code: http.StatusInternalServerError, Detail: "500"}, 4, 16, 15*time.Second)
	if got := sent.last().Message; !strings.Contains(got, "retrying in 15s, attempt 5 of 16 after: 500") {
		t.Fatalf("a server error's retry read %q", got)
	}
}

// The note ends when the retried file moves: the bytes a retry is announced
// at are not new ones.
func TestNewBytesEndTheRetryNote(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("modelscope.cn"))
	m.at(100, 200, "abc")
	m.retrying(errors.New("connection reset by peer"), 1, 16, time.Second)
	if !strings.Contains(sent.last().Message, "retrying in 1s, attempt 2 of 16") {
		t.Fatalf("message %q", sent.last().Message)
	}
	m.progress(proto.ModelFileStatus{BytesDone: 100, BytesTotal: 200})
	if !strings.Contains(sent.last().Message, "retrying in 1s, attempt 2 of 16") {
		t.Fatalf("restating the bytes already there ended the note: %q", sent.last().Message)
	}
	m.progress(proto.ModelFileStatus{BytesDone: 120, BytesTotal: 200, RateBytesPerS: 4096})
	if got := sent.last().Message; got != downloadingFrom("modelscope.cn") {
		t.Fatalf("new bytes left %q", got)
	}
}

// A long failure is quoted up to a bound, cut on a character boundary: the
// message a server stores for a model file is finite.
func TestARetryNoteBoundsTheFailureItQuotes(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("modelscope.cn"))
	m.retrying(errors.New(strings.Repeat("失", 200)), 1, 16, time.Second)
	got := sent.last().Message
	if !utf8.ValidString(got) || len(got) > 300 {
		t.Fatalf("message is %d bytes and valid=%v", len(got), utf8.ValidString(got))
	}
}

// A throttled listing (a modelrepo error, not a download one) is a wait too.
func TestAThrottledListingSaysRateLimited(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.retrying(&modelrepo.StatusError{Code: http.StatusTooManyRequests, RetryAfter: 30 * time.Second}, 2, 16, 30*time.Second)
	if !strings.Contains(sent.last().Message, "rate limited by the source; retrying in 30s, attempt 3 of 16") {
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
