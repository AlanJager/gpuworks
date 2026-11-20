package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/conn"
)

func TestReenrollAfterRejectGivesAFreshTokenOnlyForAnUnknownOne(t *testing.T) {
	calls := 0
	next := "fresh"
	var nextErr error
	a := &Agent{log: slog.New(slog.DiscardHandler), cfg: Config{Reenroll: func(context.Context) (string, error) { calls++; return next, nextErr }}}
	badToken := &conn.RejectError{Reason: "bad_token"}
	if token, ok := a.reenrollAfterReject(context.Background(), badToken, "old"); !ok || token != "fresh" {
		t.Fatalf("an unknown token = %q, %v", token, ok)
	}
	// Any other rejection is not about the token: no enrollment is asked.
	before := calls
	for _, reason := range []string{"proto_mismatch", "stale_session", ""} {
		if _, ok := a.reenrollAfterReject(context.Background(), &conn.RejectError{Reason: reason}, "old"); ok {
			t.Fatalf("a %q rejection re-enrolled", reason)
		}
	}
	if calls != before {
		t.Fatalf("enrollment was asked %d time(s) for rejections that are not about the token", calls-before)
	}
	// The same token again means the server will reject it again: wait instead of looping.
	next = "old"
	if _, ok := a.reenrollAfterReject(context.Background(), badToken, "old"); ok {
		t.Fatal("the rejected token was used again at once")
	}
	next, nextErr = "", errors.New("the server refused this node's enrollment")
	if _, ok := a.reenrollAfterReject(context.Background(), badToken, "old"); ok {
		t.Fatal("a failed enrollment gave a token")
	}
	// An agent that holds a node token of its own has nothing to enroll.
	plain := &Agent{log: slog.New(slog.DiscardHandler)}
	if _, ok := plain.reenrollAfterReject(context.Background(), badToken, "old"); ok {
		t.Fatal("an agent without an enrollment re-enrolled")
	}
}
