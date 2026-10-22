package backupexclude

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"testing"
)

func TestExcludeMarksEachExistingPathOnce(t *testing.T) {
	prevRun, prevMac, prevExcluded := run, onMac, excluded
	t.Cleanup(func() { run, onMac, excluded = prevRun, prevMac, prevExcluded })
	already := ""
	excluded = func(path string) bool { return path == already }
	var calls [][]string
	run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if args[1] == "/fail" {
			return []byte("tmutil: addexclusion requires Full Disk Access privileges"), errors.New("exit status 1")
		}
		return nil, nil
	}
	state := t.TempDir()
	models := filepath.Join(state, "models")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	onMac = true
	Exclude(log, state, state, " ", filepath.Join(state, "missing"), models)
	want := [][]string{{"tmutil", "addexclusion", state}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v (missing paths and duplicates skipped)", calls, want)
	}

	// An item already excluded is not handed to tmutil again.
	calls, already = nil, state
	Exclude(log, state)
	if len(calls) != 0 {
		t.Fatalf("tmutil ran for an excluded item: %v", calls)
	}

	calls = nil
	onMac = false
	Exclude(log, state)
	if len(calls) != 0 {
		t.Fatalf("tmutil ran off a Mac: %v", calls)
	}
}
