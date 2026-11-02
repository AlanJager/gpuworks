package gpu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Records in /dev/kmsg's format: priority, sequence, microseconds since boot,
// flags; then the message. The Xid lines are worded as the driver words them.
const kmsgSample = `6,1001,5000000,-;nvidia-modeset: Loading NVIDIA Kernel Mode Setting Driver
4,1002,7250000,-;NVRM: Xid (PCI:0000:17:00): 13, pid=4242, name=python3, Graphics SM Warp Exception on (GPC 0, TPC 1, SM 0): Out Of Range Address
4,1003,9000000,-;NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.
4,1004,9500000,-;NVRM: Xid (PCI:0000:17:00): 13, pid=4243, name=python3, Graphics SM Warp Exception
4,1005,9600000,-;NVRM: Xid (PCI:00000000:3b:00): 119, pid=1, name=nvidia-smi, Timeout after 6s of waiting for RPC response from GPU0 GSP!
`

func TestXidWatcherReadsTheKernelLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kmsg")
	if err := os.WriteFile(path, []byte(kmsgSample), 0o644); err != nil {
		t.Fatal(err)
	}
	boot := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	w := &XidWatcher{Path: path, BootTime: func() time.Time { return boot }}
	w.Run(context.Background())

	// The same Xid again keeps only its latest event.
	events := w.Events("0000:17:00.0")
	if len(events) != 1 || events[0].Xid != 13 || !events[0].At.Equal(boot.Add(9500*time.Millisecond)) {
		t.Fatalf("17:00 events = %+v", events)
	}
	// The card's address however the caller writes it: eight domain digits,
	// upper case, with or without the function.
	events = w.Events("00000000:3B:00.0")
	if len(events) != 2 || events[0].Xid != 79 || events[0].Message != "pid='<unknown>', name=<unknown>, GPU has fallen off the bus." || events[1].Xid != 119 {
		t.Fatalf("3b:00 events = %+v", events)
	}
	if got := w.Events("0000:99:00.0"); len(got) != 0 {
		t.Fatalf("a card without events = %+v", got)
	}
	var nilWatcher *XidWatcher
	if nilWatcher.Events("0000:17:00.0") != nil {
		t.Fatal("a node without a watcher has events")
	}
}

func TestXidWatcherWithoutTheLog(t *testing.T) {
	w := &XidWatcher{Path: filepath.Join(t.TempDir(), "missing")}
	done := make(chan struct{})
	go func() { w.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return when the log cannot be opened")
	}
}
