package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFetchIndexTakesTheFirstValidSource(t *testing.T) {
	slow, fast := newObjectStore(t), newObjectStore(t)
	index := testIndex("nightly")
	publish(t, index, slow, fast)
	slow.fail("nightly/index.json", fault{delay: time.Second})

	start := time.Now()
	fetched, err := clientFor(slow, fast).FetchIndex(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 700*time.Millisecond {
		t.Fatalf("FetchIndex waited %s for the slow source", elapsed)
	}
	if fetched.Source != fast.URL() || fetched.KeyID != testKeyID || fetched.Index.Server.ReleaseID != "abc1234" {
		t.Fatalf("fetched = %+v", fetched)
	}
	if len(fetched.Attempts) != 2 || fetched.Attempts[0].Source != slow.URL() || !errors.Is(fetched.Attempts[0].Err, ErrNotAwaited) {
		t.Fatalf("attempts = %+v", fetched.Attempts)
	}
	if fetched.Attempts[1].Err != nil {
		t.Fatalf("winner attempt = %+v", fetched.Attempts[1])
	}
}

func TestProbeIndexReportsEverySource(t *testing.T) {
	slow, fast, missing := newObjectStore(t), newObjectStore(t), newObjectStore(t)
	publish(t, testIndex("nightly"), slow, fast)
	slow.fail("nightly/index.json", fault{delay: 200 * time.Millisecond})

	fetched, err := clientFor(slow, fast, missing).ProbeIndex(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Source != fast.URL() {
		t.Fatalf("winner = %s, want the fast source", fetched.Source)
	}
	if fetched.Attempts[0].Err != nil || fetched.Attempts[0].Latency < 200*time.Millisecond {
		t.Fatalf("slow source should have been waited for: %+v", fetched.Attempts[0])
	}
	if err := fetched.Attempts[2].Err; err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("missing source attempt = %v", err)
	}
}

// A source whose index does not verify (a stale mirror re-signed by someone
// else, a captive portal page) loses even when it answers first.
func TestFetchIndexSkipsUnverifiedIndexes(t *testing.T) {
	forged, honest, portal := newObjectStore(t), newObjectStore(t), newObjectStore(t)
	publish(t, testIndex("nightly"), honest)
	honest.fail("nightly/index.json", fault{delay: 100 * time.Millisecond})
	payload, _ := json.Marshal(testIndex("nightly"))
	forgedEnvelope, _ := json.Marshal(Envelope{Payload: payload, Signatures: []Signature{{KeyID: testKeyID, Sig: make([]byte, 64)}}})
	forged.put("nightly/index.json", forgedEnvelope)
	portal.put("nightly/index.json", []byte("<html>login</html>"))

	fetched, err := clientFor(forged, portal, honest).FetchIndex(context.Background(), "nightly")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Source != honest.URL() {
		t.Fatalf("winner = %s", fetched.Source)
	}
}

func TestFetchIndexRefusesAnotherChannelsIndex(t *testing.T) {
	store := newObjectStore(t)
	// A release request answered with a nightly payload, e.g. a misconfigured
	// mirror that copied one channel's file over the other.
	payload, _ := json.Marshal(testIndex("nightly"))
	envelope, _ := Seal(payload, testKeyID, testPrivate)
	store.put("release/index.json", envelope)
	_, err := clientFor(store).FetchIndex(context.Background(), "release")
	var sourcesErr *SourcesError
	if !errors.As(err, &sourcesErr) || !strings.Contains(err.Error(), `channel "nightly", not "release"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchIndexWhenEverySourceFails(t *testing.T) {
	denied, missing, hung := newObjectStore(t), newObjectStore(t), newObjectStore(t)
	denied.fail("nightly/index.json", fault{status: 403})
	hung.fail("nightly/index.json", fault{delay: 5 * time.Second})
	client := clientFor(denied, missing, hung)
	client.IndexTimeout = 200 * time.Millisecond

	_, err := client.FetchIndex(context.Background(), "nightly")
	var sourcesErr *SourcesError
	if !errors.As(err, &sourcesErr) {
		t.Fatalf("err = %v, want *SourcesError", err)
	}
	if len(sourcesErr.Attempts) != 3 {
		t.Fatalf("attempts = %+v", sourcesErr.Attempts)
	}
	for i, want := range []string{"403", "404", "deadline"} {
		if got := sourcesErr.Attempts[i].Err; got == nil || !strings.Contains(got.Error(), want) {
			t.Errorf("attempt %d = %v, want %q", i, got, want)
		}
	}
	if !strings.Contains(err.Error(), denied.URL()) || !strings.Contains(err.Error(), hung.URL()) {
		t.Fatalf("error should name every source: %v", err)
	}
}

func TestFetchIndexRefusesOversizedIndex(t *testing.T) {
	store := newObjectStore(t)
	store.put("nightly/index.json", []byte(strings.Repeat(" ", maxIndexBytes+1)))
	_, err := clientFor(store).FetchIndex(context.Background(), "nightly")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchIndexHonoursCancellation(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("nightly"), store)
	store.fail("nightly/index.json", fault{delay: 5 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := clientFor(store).FetchIndex(ctx, "nightly"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
}

func TestFetchIndexRefusesBadInput(t *testing.T) {
	if _, err := (&Client{Sources: []string{"https://a.example"}}).FetchIndex(context.Background(), "Bad Channel"); err == nil {
		t.Fatal("an invalid channel must be refused before any request")
	}
	if _, err := (&Client{Sources: []string{"ftp://a.example"}}).FetchIndex(context.Background(), "nightly"); err == nil {
		t.Fatal("an invalid source must be refused")
	}
}

// Why no source served an index decides what the console tells the
// administrator: an empty channel, a damaged answer, or no way through.
func TestSourcesErrorReason(t *testing.T) {
	forgedIndex := func(s *objectStore) {
		payload, _ := json.Marshal(testIndex("release"))
		forged, _ := json.Marshal(Envelope{Payload: payload, Signatures: []Signature{{KeyID: testKeyID, Sig: make([]byte, 64)}}})
		s.put("release/index.json", forged)
	}
	for _, tc := range []struct {
		name    string
		prepare func(a, b *objectStore)
		want    string
	}{
		{"every source says not found", func(a, b *objectStore) {}, ReasonUnpublished},
		{"one says not found, the other cannot be reached", func(a, b *objectStore) {
			b.fail("release/index.json", fault{delay: 5 * time.Second})
		}, ReasonUnpublished},
		{"a captive portal and a server error", func(a, b *objectStore) {
			a.put("release/index.json", []byte("<html>sign in to the network</html>"))
			b.fail("release/index.json", fault{status: http.StatusBadGateway})
		}, ReasonInvalid},
		{"a forged index and a denied request", func(a, b *objectStore) {
			forgedIndex(a)
			b.fail("release/index.json", fault{status: http.StatusForbidden})
		}, ReasonInvalid},
		{"denied, server errors and timeouts", func(a, b *objectStore) {
			a.fail("release/index.json", fault{status: http.StatusForbidden})
			b.fail("release/index.json", fault{delay: 5 * time.Second})
		}, ReasonUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := newObjectStore(t), newObjectStore(t)
			tc.prepare(a, b)
			client := clientFor(a, b)
			client.IndexTimeout = 300 * time.Millisecond
			_, err := client.FetchIndex(context.Background(), "release")
			var sourcesErr *SourcesError
			if !errors.As(err, &sourcesErr) {
				t.Fatalf("err = %v, want *SourcesError", err)
			}
			if got := sourcesErr.Reason(); got != tc.want {
				t.Fatalf("reason = %q, want %q (%v)", got, tc.want, err)
			}
		})
	}
	// A source that cannot be dialled at all is unreachable too.
	client := &Client{Sources: []string{"http://127.0.0.1:1"}, Keys: testKeys(), IndexTimeout: time.Second}
	_, err := client.FetchIndex(context.Background(), "release")
	var sourcesErr *SourcesError
	if !errors.As(err, &sourcesErr) || sourcesErr.Reason() != ReasonUnreachable {
		t.Fatalf("refused connection: %v", err)
	}
}
