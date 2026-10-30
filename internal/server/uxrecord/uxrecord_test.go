package uxrecord

import (
	"context"
	"testing"
)

func TestTokenBucketEdges(t *testing.T) {
	cases := map[int32]int{-5: 0, 0: 0, 1: 0, 2: 1, 3: 1, 4: 2, 1023: 9, 1024: 10, 1 << 20: 20, 1 << 30: TokenBuckets - 1}
	for tokens, want := range cases {
		if got := TokenBucket(tokens); got != want {
			t.Errorf("TokenBucket(%d) = %d, want %d", tokens, got, want)
		}
	}
	if TokenBucketLow(0) != 0 || TokenBucketLow(10) != 1024 {
		t.Fatalf("TokenBucketLow edges wrong")
	}
}

func TestTTFTBucketEdges(t *testing.T) {
	if TTFTBucket(0) != 0 || TTFTBucket(99) != 0 || TTFTBucket(100) != 1 {
		t.Fatalf("first buckets wrong")
	}
	if got := TTFTBucket(1 << 30); got != TTFTBuckets-1 {
		t.Fatalf("overflow bucket = %d, want %d", got, TTFTBuckets-1)
	}
}

func TestFirstTokenAndUncached(t *testing.T) {
	r := Request{FirstThinkMS: Unknown, FirstAnswerMS: 300, Usage: true, PromptTokens: 1000, CachedTokens: 600}
	if r.FirstTokenMS() != 300 || r.UncachedTokens() != 400 {
		t.Fatalf("answer-only: first=%d uncached=%d", r.FirstTokenMS(), r.UncachedTokens())
	}
	r.FirstThinkMS = 120
	if r.FirstTokenMS() != 120 {
		t.Fatalf("thinking first token = %d, want 120", r.FirstTokenMS())
	}
	r.FirstThinkMS, r.FirstAnswerMS = Unknown, Unknown
	if r.FirstTokenMS() != Unknown {
		t.Fatalf("no token should be Unknown")
	}
	r.CachedTokens = Unknown
	if r.UncachedTokens() != Unknown {
		t.Fatalf("unknown cache must give Unknown uncached")
	}
}

func TestGridCells(t *testing.T) {
	r := Request{Usage: false}
	if c := GridCells(&r); c.HasIO || c.HasUncachedTT {
		t.Fatalf("no usage must add no cells: %+v", c)
	}
	r = Request{Usage: true, PromptTokens: 5000, CompletionTokens: 300, CachedTokens: Unknown, FirstThinkMS: Unknown, FirstAnswerMS: 800}
	c := GridCells(&r)
	if !c.HasIO || c.HasUncachedTT || c.IO.X != 12 || c.IO.Y != 8 {
		t.Fatalf("unknown cache: %+v", c)
	}
	r.CachedTokens = 4000
	c = GridCells(&r)
	if !c.HasUncachedTT || c.UncachedTTFT.X != 9 || c.UncachedTTFT.Y != TTFTBucket(800) {
		t.Fatalf("uncached cell: %+v", c)
	}
}

func TestCallerRoundTripsThroughContext(t *testing.T) {
	if _, ok := CallerFrom(context.Background()); ok {
		t.Fatal("an empty context has no caller")
	}
	want := Caller{APIKeyID: 4, UserID: 2, Source: SourcePlayground, ReceivedMS: 1700}
	got, ok := CallerFrom(WithCaller(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("caller = %+v %v, want %+v", got, ok, want)
	}
}

func TestSourceNames(t *testing.T) {
	for source, want := range map[Source]string{SourceAPI: "api", SourcePlayground: "playground", SourceBatch: "batch", Source(9): "api"} {
		if got := source.String(); got != want {
			t.Errorf("Source(%d) = %q, want %q", source, got, want)
		}
	}
}
