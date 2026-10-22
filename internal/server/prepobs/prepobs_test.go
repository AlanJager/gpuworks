package prepobs

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func copyFrame(key, transfer string, done int64) proto.ModelFileStatus {
	return proto.ModelFileStatus{Key: key, TransferID: transfer, State: proto.MFDownloading, BytesTotal: 100, BytesDone: done,
		Preparation: &proto.ModelPreparation{Attempt: "a", Phase: proto.ModelPhaseCopying, TotalKnown: true}}
}

func TestCopiesAreBoundToTheRowsCurrentIntent(t *testing.T) {
	c := NewCopies()
	now := time.Unix(1000, 0)
	c.Record(1, copyFrame("7", "t1", 40), now)
	got, ok := c.Observe("7", proto.MFDownloading, "t1", now)
	if !ok || got.BytesDone != 40 || got.Preparation.Phase != proto.ModelPhaseCopying {
		t.Fatalf("observation=%+v ok=%v", got, ok)
	}
	got.Preparation.Phase = "mutated"
	if again, _ := c.Observe("7", proto.MFDownloading, "t1", now); again.Preparation.Phase != proto.ModelPhaseCopying {
		t.Fatal("reader mutated the stored observation")
	}
	for _, tc := range []struct{ state, transfer string }{{proto.MFDownloading, "t2"}, {proto.MFFailed, "t1"}, {proto.MFPresent, "t1"}} {
		if _, ok := c.Observe("7", tc.state, tc.transfer, now); ok {
			t.Fatalf("observation leaked to row state=%s transfer=%s", tc.state, tc.transfer)
		}
	}
	if _, ok := c.Observe("7", proto.MFDownloading, "t1", now.Add(CopyTTL+time.Second)); ok {
		t.Fatal("expired observation visible")
	}
	terminal := copyFrame("7", "t1", 100)
	terminal.State, terminal.Preparation = proto.MFPresent, nil
	c.Record(1, terminal, now)
	if _, ok := c.Observe("7", proto.MFDownloading, "t1", now); ok || len(c.byNode) != 0 {
		t.Fatal("terminal frame did not retire the observation and its index")
	}
}

func TestCopiesForgetNodeOnlyDropsThatNode(t *testing.T) {
	c := NewCopies()
	now := time.Unix(1000, 0)
	const nodes, copies = 1000, 3000
	for i := 0; i < copies; i++ {
		c.Record(uint(1+i%nodes), copyFrame(fmt.Sprint(i), "t", int64(i)), now)
	}
	if len(c.entries.entries) != copies || len(c.byNode) != nodes {
		t.Fatalf("entries=%d nodes=%d", len(c.entries.entries), len(c.byNode))
	}
	c.ForgetNode(1)
	if len(c.entries.entries) != copies-3 || c.byNode[1] != nil {
		t.Fatal("disconnect removed other nodes or kept its own copies")
	}
	for _, key := range []string{"0", "1000", "2000"} {
		if _, ok := c.Observe(key, proto.MFDownloading, "t", now); ok {
			t.Fatalf("copy %s of the disconnected node is still observed", key)
		}
	}
	if got, ok := c.Observe("1", proto.MFDownloading, "t", now); !ok || got.BytesDone != 1 {
		t.Fatal("another node's observation lost")
	}
	// A copy moving to another node is indexed under the new reporter only.
	c.Record(9, copyFrame("1", "t", 2), now)
	c.ForgetNode(2)
	if _, ok := c.Observe("1", proto.MFDownloading, "t", now); !ok {
		t.Fatal("the previous reporter's disconnect dropped the moved copy")
	}
}

func TestCopiesCapacityKeepsExistingAndThrottlesSweeps(t *testing.T) {
	c := NewCopies()
	start := time.Unix(1000, 0)
	for i := 0; i < CopyLimit; i++ {
		c.Record(1, copyFrame(fmt.Sprint(i), "t", 0), start)
	}
	c.Record(2, copyFrame("new", "t", 0), start)
	sweep := c.entries.lastSweep
	if sweep.IsZero() || len(c.entries.entries) != CopyLimit {
		t.Fatal("capacity guard not enforced")
	}
	if _, ok := c.Observe("new", proto.MFDownloading, "t", start); ok || c.byNode[2] != nil {
		t.Fatal("rejected admission left an observation or index")
	}
	c.Record(1, copyFrame("0", "t", 42), start)
	c.Record(2, copyFrame("other", "t", 0), start)
	if got, _ := c.Observe("0", proto.MFDownloading, "t", start); got.BytesDone != 42 || c.entries.lastSweep != sweep {
		t.Fatal("existing observation lost or full sweep repeated")
	}
	later := start.Add(CopyTTL + time.Second)
	c.Record(2, copyFrame("fresh", "t", 0), later)
	if _, ok := c.Observe("fresh", proto.MFDownloading, "t", later); !ok {
		t.Fatal("expired entries were not reclaimed for a new copy")
	}
	if len(c.byNode[1]) != 0 || len(c.entries.entries) != 1 {
		t.Fatalf("sweep left index entries: node1=%d entries=%d", len(c.byNode[1]), len(c.entries.entries))
	}
}

func TestPhasesExpireAndRetire(t *testing.T) {
	p := NewPhases[int]()
	now := time.Unix(1000, 0)
	p.Set(1, PhaseEvicting, now)
	if p.Get(1, now) != PhaseEvicting {
		t.Fatal("phase not recorded")
	}
	if p.Get(1, now.Add(PhaseTTL+time.Second)) != "" {
		t.Fatal("expired phase visible")
	}
	p.Set(1, PhaseEvictionFailed, now)
	p.Set(1, "", now)
	if p.Get(1, now) != "" {
		t.Fatal("empty phase did not retire")
	}
}
