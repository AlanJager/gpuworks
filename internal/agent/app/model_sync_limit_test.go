package app

import (
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestModelArchiveLimit(t *testing.T) {
	t.Run("small archive keeps headroom", func(t *testing.T) {
		const expected int64 = 100 << 30
		want := expected + expected/20 + 1<<30
		if got := modelArchiveLimit(expected); got != want {
			t.Fatalf("limit = %d, want %d", got, want)
		}
	})
	t.Run("Kimi K3.1 Pro fits", func(t *testing.T) {
		const expected int64 = 1466844769783
		got := modelArchiveLimit(expected)
		if got <= expected {
			t.Fatalf("limit = %d, expected archive bytes = %d", got, expected)
		}
		if want := expected + expected/20 + 1<<30; got != want {
			t.Fatalf("limit = %d, want expected plus headroom = %d", got, want)
		}
	})
	t.Run("unknown size is bounded by the hard limit", func(t *testing.T) {
		if got := modelArchiveLimit(0); got != modelArchiveHardLimit {
			t.Fatalf("limit = %d, want %d", got, modelArchiveHardLimit)
		}
	})
	t.Run("huge size does not overflow", func(t *testing.T) {
		if got := modelArchiveLimit(math.MaxInt64); got != modelArchiveHardLimit {
			t.Fatalf("limit = %d, want %d", got, modelArchiveHardLimit)
		}
	})
}

func TestModelArchiveFits(t *testing.T) {
	largestFitting := (modelArchiveHardLimit - 1<<30) * 20 / 21
	for _, tc := range []struct {
		name     string
		expected int64
		fits     bool
	}{
		{"unknown size", 0, true},
		{"Kimi K3.1 Pro", 1466844769783, true},
		{"largest size with headroom", largestFitting, true},
		{"headroom crosses the limit", modelArchiveHardLimit - 1<<30, false},
		{"exactly the hard limit", modelArchiveHardLimit, false},
		{"far above the limit", math.MaxInt64, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := modelArchiveFits(tc.expected); got != tc.fits {
				t.Fatalf("fits(%d) = %v, want %v", tc.expected, got, tc.fits)
			}
		})
	}
}

// go test ./internal/agent/app -run '^TestModelSyncListenRefusesModelAboveTransferLimit$' -count=1
func TestModelSyncListenRefusesModelAboveTransferLimit(t *testing.T) {
	node := &NodeRuntime{
		log: slog.New(slog.NewTextHandler(io.Discard, nil)), modelsDir: t.TempDir(),
		modelSyncs: map[string]*activeModelSync{}, progress: newProgressGate(),
	}
	var statuses []proto.ModelFileStatus
	node.Bind(func(kind string, data any) error {
		if kind == proto.TStatusModelFile {
			statuses = append(statuses, data.(proto.ModelFileStatus))
		}
		return nil
	})
	_, err := node.rpcModelSyncListen(mustRaw(t, proto.ModelSyncListenParams{
		Key: "1", TransferID: "too-large", TargetPath: "model", Token: "secret", BindIP: "127.0.0.1",
		ExpectedFileBytes: modelArchiveHardLimit,
	}))
	if err == nil || !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("listen err = %v, want the transfer limit refusal", err)
	}
	// Refused before listening: no receiver is registered and the model file
	// never enters syncing.
	if len(node.modelSyncs) != 0 || len(statuses) != 0 {
		t.Fatalf("refused listen left syncs=%v statuses=%+v", node.modelSyncs, statuses)
	}
}
