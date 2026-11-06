//go:build linux

package serverconfig_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

func TestOutboundPrivateHostsWatchRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.yaml")
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [first.internal]\n")
	read := serverconfig.WatchOutboundPrivateHosts(path, nil, nil)
	assertOutboundHosts(t, read, "first.internal")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	result := make(chan []string, 1)
	go func() { result <- read() }()
	select {
	case hosts := <-result:
		if len(hosts) != 0 {
			t.Fatalf("FIFO configuration retained private grants: %v", hosts)
		}
	case <-time.After(time.Second):
		t.Error("configuration FIFO blocked waiting for a writer; runtime policy must reject it immediately")
		// Release the old blocking implementation so the failing regression itself
		// does not leave a blocked goroutine behind.
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			t.Fatalf("release blocked FIFO reader: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-result:
		case <-time.After(time.Second):
			t.Fatal("FIFO reader did not exit after release")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeOutboundConfig(t, path, "outbound_allow_private_hosts: [restored.internal]\n")
	assertOutboundHosts(t, read, "restored.internal")
}
