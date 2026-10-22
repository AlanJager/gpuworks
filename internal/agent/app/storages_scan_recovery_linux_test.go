//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStorageScanRecoversAfterTimedOutReadFinishes(t *testing.T) {
	r := newStorageTestRuntime(t)
	r.storage.timeouts.scanGrace = 50 * time.Millisecond
	shared := t.TempDir()
	model := filepath.Join(shared, "org", "model")
	writeModel(t, model, "weights")
	marker := filepath.Join(model, modelscan.CompletionMarker)
	if err := syscall.Mkfifo(marker, 0600); err != nil {
		t.Fatal(err)
	}
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)

	request := mustRaw(t, proto.StorageScanParams{StorageID: 2, TimeoutSeconds: 1})
	done := make(chan error, 1)
	go func() { _, err := r.rpcStorageScan(context.Background(), request); done <- err }()
	var fd int
	until := time.Now().Add(5 * time.Second)
	for {
		var err error
		fd, err = syscall.Open(marker, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("scan did not reach marker read")
		}
		time.Sleep(time.Millisecond)
	}
	writer := os.NewFile(uintptr(fd), marker)
	defer writer.Close()
	select {
	case err := <-done:
		if !errors.Is(err, storagemount.ErrUnresponsive) {
			t.Fatalf("timeout=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not return at its guard deadline")
	}
	// The old read continues, so retries must not pile up another scan.
	if _, err := r.rpcStorageScan(context.Background(), request); !errors.Is(err, storagemount.ErrUnresponsive) {
		t.Fatalf("scan still in progress: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	// EOF lets the original read finish naturally; no cancellation is sent.
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	until = time.Now().Add(5 * time.Second)
	for {
		raw, err := r.rpcStorageScan(context.Background(), request)
		if err == nil {
			page := raw.(proto.StorageScanResult)
			if !page.Complete || len(page.Candidates) != 1 || page.Candidates[0].RelPath != "org/model" {
				t.Fatalf("recovered scan lost model: %+v", page)
			}
			break
		}
		if !errors.Is(err, storagemount.ErrUnresponsive) || time.Now().After(until) {
			t.Fatalf("scan did not recover: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}
