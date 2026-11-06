package hub

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/outbound"
)

func TestArchiveRelayHonorsRuntimePrivatePermissionChanges(t *testing.T) {
	h := newRelayHub(t, "file:relay_runtime_policy?mode=memory&cache=shared", "/image.tar")
	var hosts []string
	h.hub.SetOutboundPrivateHostsReader(func() []string { return hosts })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, allowed := range []bool{false, true, false} {
		hosts = nil
		if allowed {
			hosts = []string{"127.0.0.1"}
		}
		n, err := h.hub.CopyImageArchiveURL(ctx, h.url, h.node.UUID, h.image.ImageRef, "runtime-permission", nil)
		if !allowed {
			if !errors.Is(err, outbound.ErrTargetDenied) || n != 0 {
				t.Fatalf("unapproved relay: copied=%d err=%v", n, err)
			}
			select {
			case header := <-h.headers:
				t.Fatalf("unapproved relay opened an agent stream: %+v", header)
			default:
			}
			continue
		}
		if err != nil || n != int64(len(h.archive)) {
			t.Fatalf("allowed relay: copied=%d err=%v", n, err)
		}
		select {
		case body := <-h.loaded:
			if !bytes.Equal(body, h.archive) {
				t.Fatal("agent did not receive the original archive")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-h.headers:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	h.assertNothingStaged(t)
}
