package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/agent/download"
	"gpuworks.ai/gpuworks/internal/agent/modelfetch"
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

// While the source has asked the download to wait, the status carries that
// retry, marked as the source's wait, with the progress already reached;
// bytes beyond that point put the address back. The message itself says only
// where the bytes come from (GWKS-526).
func TestTheStatusSaysWhenTheSourceAskedTheDownloadToWait(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.progress(proto.ModelFileStatus{Key: "k", State: proto.MFDownloading, BytesDone: 40 << 30, BytesTotal: 60 << 30, RateBytesPerS: 1 << 20, Revision: "abc"})
	m.retrying("tokenizer.json", throttle(110*time.Second), 1, 16, 110*time.Second)

	got := sent.last()
	if got.Message != downloadingFrom("huggingface.co") {
		t.Fatalf("message %q, want the address alone", got.Message)
	}
	want := proto.ModelFetchRetry{Attempt: 2, Attempts: 16, WaitSeconds: 110, Throttled: true, File: "tokenizer.json"}
	if got.Retry == nil || *got.Retry != want {
		t.Fatalf("retry %+v, want %+v", got.Retry, want)
	}
	if got.BytesDone != 40<<30 || got.BytesTotal != 60<<30 || got.Revision != "abc" || got.RateBytesPerS != 0 {
		t.Fatalf("the wait was announced as %+v: the progress reached must stay, with no rate", got)
	}

	// A resumed file reports the bytes already on disk first: not new bytes.
	m.progress(proto.ModelFileStatus{BytesDone: 40 << 30, BytesTotal: 60 << 30})
	if sent.last().Retry == nil {
		t.Fatal("a report of the bytes already there ended the wait")
	}
	m.progress(proto.ModelFileStatus{BytesDone: 40<<30 + 4096, BytesTotal: 60 << 30})
	if sent.last().Retry != nil {
		t.Fatalf("new bytes left the wait %+v", sent.last().Retry)
	}
}

// Other retries (a reset, a server error) are not waits the source asked
// for, and are reported as the retry they are: a download that keeps failing
// near its end must not read as one that stopped (GWKS-526).
func TestOtherRetriesAreReportedAsARetryNotAsASourcesWait(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("hf-mirror.com"))
	m.at(14_598_320_397, 14_606_988_015, "abc")
	m.retrying("tokenizer.json", fmt.Errorf("download tokenizer.json: %w", errors.New("read tcp 10.0.0.2:60286->39.99.133.195:443: read: connection reset by peer")), 3, 16, 8*time.Second)
	got := sent.last()
	if got.Message != downloadingFrom("hf-mirror.com") {
		t.Fatalf("message %q", got.Message)
	}
	want := proto.ModelFetchRetry{Attempt: 4, Attempts: 16, WaitSeconds: 8, Cause: "read tcp 10.0.0.2:60286->39.99.133.195:443: read: connection reset by peer", File: "tokenizer.json"}
	if got.Retry == nil || *got.Retry != want {
		t.Fatalf("retry %+v, want %+v", got.Retry, want)
	}
	if got.BytesDone != 14_598_320_397 || got.BytesTotal != 14_606_988_015 || got.RateBytesPerS != 0 {
		t.Fatalf("the retry was announced as %+v: the bytes reached must stay, with no rate", got)
	}
	m.retrying("tokenizer.json", &download.StatusError{Code: http.StatusInternalServerError, Detail: "500"}, 4, 16, 15*time.Second)
	if r := sent.last().Retry; r == nil || r.Attempt != 5 || r.Throttled || r.Cause == "" {
		t.Fatalf("a server error's retry is %+v", r)
	}
}

// A throttled listing (a modelrepo error, not a download one) is a wait too.
func TestAThrottledListingIsAWaitToo(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.retrying(modelfetch.ListingRetry, &modelrepo.StatusError{Code: http.StatusTooManyRequests, RetryAfter: 30 * time.Second}, 2, 16, 30*time.Second)
	if r := sent.last().Retry; r == nil || !r.Throttled || !r.Listing || r.File != "" || r.WaitSeconds != 30 || r.Attempt != 3 {
		t.Fatalf("retry %+v", r)
	}
}

// A retry after a switch keeps the switch's message as it is: the wait is the
// retry's, so nothing is appended after the switch reason, which the console
// reads as one parenthetical.
func TestARetryAfterASwitchKeepsTheSwitchedMessage(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("modelscope.cn"))
	m.at(50, 100, "abc")
	switched := downloadingFrom("huggingface.co") + " (switched: modelscope.cn failed: listing after 6 attempt(s))"
	m.set(switched)
	m.retrying("tokenizer.json", throttle(30*time.Second), 1, 16, 30*time.Second)
	m.retrying("tokenizer.json", errors.New("read: connection reset by peer"), 2, 16, 2*time.Second)
	got := sent.last()
	if got.Message != switched {
		t.Fatalf("message %q, want %q", got.Message, switched)
	}
	if got.Retry == nil || got.Retry.Throttled || got.Retry.Attempt != 3 || got.Retry.WaitSeconds != 2 {
		t.Fatalf("retry %+v", got.Retry)
	}
}

// The retry ends when the retried file moves: the bytes it was announced at
// are not new ones. Moving to another address ends it too.
func TestNewBytesOrASwitchEndTheRetry(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("modelscope.cn"))
	m.at(100, 200, "abc")
	m.retrying("tokenizer.json", errors.New("connection reset by peer"), 1, 16, time.Second)
	m.progress(proto.ModelFileStatus{BytesDone: 100, BytesTotal: 200})
	if sent.last().Retry == nil {
		t.Fatal("restating the bytes already there ended the retry")
	}
	m.progress(proto.ModelFileStatus{BytesDone: 120, BytesTotal: 200, RateBytesPerS: 4096})
	if sent.last().Retry != nil {
		t.Fatalf("new bytes left the retry %+v", sent.last().Retry)
	}
	m.retrying("tokenizer.json", errors.New("connection reset by peer"), 1, 6, time.Second)
	m.set(downloadingFrom("hf-mirror.com") + " (switched: modelscope.cn failed 6 times)")
	if sent.last().Retry != nil {
		t.Fatal("the retry of the address before outlived the switch")
	}
}

// The cause is the innermost failure, cut on a character boundary; a throttle
// carries none, its wait says it all.
func TestARetryCauseIsTheInnermostFailureBounded(t *testing.T) {
	if got := retryCause(fmt.Errorf("download a: %w", fmt.Errorf("get b: %w", errors.New("read: connection reset by peer")))); got != "read: connection reset by peer" {
		t.Fatalf("cause %q", got)
	}
	cause := retryCause(errors.New(strings.Repeat("失", 200)))
	if !utf8.ValidString(cause) || len(cause) > retryCauseLimit+len("…") {
		t.Fatalf("cause is %d bytes, valid=%v", len(cause), utf8.ValidString(cause))
	}
	if retryCause(nil) != "" {
		t.Fatal("a nil failure has no cause")
	}
}

// Moving to another address ends the retry the last address had, and the
// switch is announced with the progress reached.
func TestASwitchEndsTheRetryOfTheAddressBefore(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("hf-mirror.com"))
	m.at(7<<20, 9<<20, "abc")
	m.retrying("tokenizer.json", throttle(5*time.Second), 1, 2, 5*time.Second)
	m.set(downloadingFrom("huggingface.co") + " (switched: hf-mirror.com failed 6 times)")
	got := sent.last()
	if got.Retry != nil || !strings.HasPrefix(got.Message, "downloading from huggingface.co") {
		t.Fatalf("after the switch: %+v", got)
	}
	if got.BytesDone != 7<<20 || got.BytesTotal != 9<<20 {
		t.Fatalf("the switch was announced as %+v", got)
	}
}

// A retry of the file list says so as a flag, with no path: the listing has
// none, and its label is the agent's, not a wire value.
func TestAListingRetryIsFlaggedWithoutAPath(t *testing.T) {
	m, sent := newTestFetchMessage()
	m.set(downloadingFrom("huggingface.co"))
	m.retrying(modelfetch.ListingRetry, errors.New("read: connection reset by peer"), 1, 16, time.Second)
	if r := sent.last().Retry; r == nil || !r.Listing || r.File != "" {
		t.Fatalf("retry %+v, want listing with no file", r)
	}
	wire, err := json.Marshal(sent.last().Retry)
	if err != nil || string(wire) != `{"attempt":2,"attempts":16,"wait_seconds":1,"cause":"read: connection reset by peer","listing":true}` {
		t.Fatalf("wire %s err=%v", wire, err)
	}
}

// A path from the source is bounded from the front and cannot carry control
// or bidi-override characters into the line it is shown in.
func TestARetryFileIsBoundedAndPrintable(t *testing.T) {
	long := strings.Repeat("dir/", 100) + "weights.safetensors"
	got := retryFile(long)
	if len(got) > retryFileLimit+len("…") || !strings.HasSuffix(got, "weights.safetensors") || !strings.HasPrefix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("long path -> %q", got)
	}
	if got := retryFile("a\nb\u202Ec.png"); strings.ContainsAny(got, "\n\u202E") {
		t.Fatalf("control characters kept: %q", got)
	}
	// A line separator is no control character, yet some renderings break
	// the line at it; a mark (U+200F) is a bidi control all the same.
	if got := retryFile("a\u2028b\u2029c\u200F.png"); got != "a\uFFFDb\uFFFDc\uFFFD.png" {
		t.Fatalf("separators and marks kept: %q", got)
	}
	if got := retryFile("assets/dsv4_performance.png"); got != "assets/dsv4_performance.png" {
		t.Fatalf("plain path changed: %q", got)
	}
}
