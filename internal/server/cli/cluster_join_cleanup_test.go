package cli

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

type joinFailedOutput struct{ err error }

func (w joinFailedOutput) Write([]byte) (int, error) { return 0, w.err }

func TestClusterJoinWaitPreservesEncodeAndCloseFailures(t *testing.T) {
	encodeErr := errors.New("output disconnected")
	closeErr := errors.New("native shutdown failed")
	calls := 0
	err := clusterJoinWait(t.Context(), joinFailedOutput{encodeErr}, map[string]string{"address": "local"}, func() error { calls++; return closeErr })
	if !errors.Is(err, encodeErr) || !errors.Is(err, closeErr) || calls != 1 {
		t.Fatalf("join lost cleanup error: %v calls%d", err, calls)
	}
}

func TestClusterJoinWaitNormalCancellationRequiresCleanClose(t *testing.T) {
	for _, fail := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		var output bytes.Buffer
		closeErr := errors.New("native close failure")
		calls := 0
		err := clusterJoinWait(ctx, &output, map[string]string{"address": "local"}, func() error {
			calls++
			if fail {
				return closeErr
			}
			return nil
		})
		if output.String() != "{\"address\":\"local\"}\n" || calls != 1 || fail && !errors.Is(err, closeErr) || !fail && err != nil {
			t.Fatalf("join wait=%v output=%q calls%d", err, output.String(), calls)
		}
	}
}
