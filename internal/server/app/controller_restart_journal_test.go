//go:build linux && dqlite

package app

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	journalStarted   = `level=INFO msg="starting controller on local database leader"`
	journalRestarted = `level=INFO msg="restarting controller on local database leader"`
	journalStopped   = `level=INFO msg="stopping controller after local scheduling role lost"`
	// What the acceptance readers look for: the bare text, anywhere in a line.
	readerStart = "starting controller on local database leader"
)

func superviseForJournal(t *testing.T, held bool, observe func(context.Context) (bool, error), onRole func(bool) error) *syncedLog {
	t.Helper()
	journal := &syncedLog{}
	log := slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseControllerFrom(ctx, held, observe, func(ctx context.Context) { <-ctx.Done() }, log, time.Millisecond, time.Second, onRole, nil)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return journal
}

// A supervisor that replaces one which ended with the role journals a
// restart, once. A reader that looks for the start text still finds exactly
// one line, and a later loss and regain are journaled as stop and start.
func TestControllerReplacingAHeldRoleJournalsOneRestart(t *testing.T) {
	var mu sync.Mutex
	leader := true
	observe := func(context.Context) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return leader, nil
	}
	setLeader := func(v bool) { mu.Lock(); leader = v; mu.Unlock() }
	journal := superviseForJournal(t, true, observe, nil)
	awaitLog(t, journal, journalRestarted, 1)
	time.Sleep(20 * time.Millisecond)
	if journal.count(journalRestarted) != 1 || journal.count(journalStarted) != 0 || journal.count(journalStopped) != 0 || journal.count(readerStart) != 1 {
		t.Fatalf("held role was not journaled as one restart:\n%s", journal.String())
	}
	setLeader(false)
	awaitLog(t, journal, journalStopped, 1)
	setLeader(true)
	awaitLog(t, journal, journalStarted, 1)
	if journal.count(journalRestarted) != 1 || journal.count(readerStart) != 2 {
		t.Fatalf("regained role was journaled as a restart:\n%s", journal.String())
	}
}

// A role the earlier supervisor held and this one never sees is journaled and
// recorded as lost once, so the journal does not end on a start that no
// longer holds; a later start is an ordinary start.
func TestControllerReplacingAHeldRoleJournalsItsLoss(t *testing.T) {
	var mu sync.Mutex
	leader := false
	observe := func(context.Context) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return leader, nil
	}
	var lost, gained atomic.Int64
	onRole := func(v bool) error {
		if v {
			gained.Add(1)
		} else {
			lost.Add(1)
		}
		return nil
	}
	journal := superviseForJournal(t, true, observe, onRole)
	awaitLog(t, journal, journalStopped, 1)
	time.Sleep(20 * time.Millisecond)
	if journal.count(journalStopped) != 1 || lost.Load() != 1 || gained.Load() != 0 || journal.count(readerStart) != 0 {
		t.Fatalf("lost held role: stops=%d recorded=%d gained=%d\n%s", journal.count(journalStopped), lost.Load(), gained.Load(), journal.String())
	}
	mu.Lock()
	leader = true
	mu.Unlock()
	awaitLog(t, journal, journalStarted, 1)
	if journal.count(journalRestarted) != 0 {
		t.Fatalf("start after a journaled loss was a restart:\n%s", journal.String())
	}
}

// A first supervisor, and one that replaces a follower's, journal nothing new.
func TestControllerWithoutAHeldRoleJournalsAsBefore(t *testing.T) {
	a := &App{}
	if a.inferenceRoleHeld() {
		t.Fatal("a new process holds a role")
	}
	a.noteInferenceRole(true)
	if !a.inferenceRoleHeld() {
		t.Fatal("recorded leadership is not held")
	}
	a.noteInferenceRole(false)
	if a.inferenceRoleHeld() {
		t.Fatal("recorded loss is still held")
	}
	journal := superviseForJournal(t, false, func(context.Context) (bool, error) { return false, nil }, nil)
	time.Sleep(20 * time.Millisecond)
	if journal.count(readerStart) != 0 || journal.count(journalStopped) != 0 {
		t.Fatalf("follower journaled a role:\n%s", journal.String())
	}
}
