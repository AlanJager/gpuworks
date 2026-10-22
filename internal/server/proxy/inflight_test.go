package proxy

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestLiveTableSnapshotOrdersAndCountsPhases(t *testing.T) {
	var table liveTable
	base := time.Unix(1000, 0)
	mk := func(id string, receivedS int, think, answer bool) *liveRequest {
		l := &liveRequest{id: id, deploymentID: 7, receivedAt: base.Add(time.Duration(receivedS) * time.Second), dispatchedAt: base.Add(time.Duration(receivedS)*time.Second + 50*time.Millisecond)}
		if think {
			l.noteEvent(200*time.Millisecond, true, false)
		}
		if answer {
			l.noteEvent(900*time.Millisecond, false, true)
		}
		table.add(l)
		return l
	}
	mk("answer-old", 1, true, true)
	mk("wait-new", 9, false, false)
	mk("think", 3, true, false)
	mk("wait-old", 2, false, false)
	other := &liveRequest{id: "other-deployment", deploymentID: 8, receivedAt: base}
	table.add(other)

	snap := table.snapshot(7, 10, base.Add(10*time.Second))
	if snap.Waiting != 2 || snap.Thinking != 1 || snap.Answering != 1 {
		t.Fatalf("counts = %+v", snap)
	}
	var order []string
	for _, r := range snap.Requests {
		order = append(order, r.ID)
	}
	if fmt.Sprint(order) != "[wait-old wait-new think answer-old]" {
		t.Fatalf("order = %v", order)
	}
	think := snap.Requests[2]
	if think.DispatchMS != 50 || think.FirstThinkMS != 250 || think.FirstAnswerMS != uxrecord.Unknown || think.Events != 1 || think.ThinkEvents != 1 || think.AgeMS != 7000 {
		t.Fatalf("thinking request = %+v", think)
	}
	if snap.Requests[3].Phase() != uxrecord.PhaseAnswering || snap.Requests[3].FirstAnswerMS != 950 {
		t.Fatalf("answering request = %+v", snap.Requests[3])
	}
	if limited := table.snapshot(7, 2, base); len(limited.Requests) != 2 || limited.Waiting != 2 || limited.Answering != 1 {
		t.Fatalf("a limit must cut the list, not the counts: %+v", limited)
	}
	table.remove(other)
	if empty := table.snapshot(8, 10, base); len(empty.Requests) != 0 {
		t.Fatalf("removed request still listed: %+v", empty)
	}
	var none *liveRequest
	none.noteEvent(time.Second, true, true) // an observer outside the table
}

func TestLiveTableIsSafeUnderConcurrentUse(t *testing.T) {
	var table liveTable
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l := &liveRequest{id: fmt.Sprint(g, i), deploymentID: uint(g % 3), receivedAt: time.Now(), dispatchedAt: time.Now()}
				table.add(l)
				l.noteEvent(time.Millisecond, i%2 == 0, i%3 == 0)
				table.remove(l)
			}
		}(g)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = table.snapshot(uint(g%3), 5, time.Now())
			}
		}(g)
	}
	wg.Wait()
	for dep := uint(0); dep < 3; dep++ {
		if snap := table.snapshot(dep, 0, time.Now()); len(snap.Requests) != 0 {
			t.Fatalf("deployment %d left %d entries", dep, len(snap.Requests))
		}
	}
}
