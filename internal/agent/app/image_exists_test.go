package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type presenceDriver struct {
	runtime.Driver
	exists bool
	err    error
	ref    string
}

func (d *presenceDriver) ImageExists(_ context.Context, ref string) (bool, error) {
	d.ref = ref
	return d.exists, d.err
}

func (d *presenceDriver) Name() string { return "presence-test" }

func TestImageExistsRPC(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exists bool
		err    error
	}{
		{"present", true, nil}, {"absent", false, nil}, {"runtime failure", false, errors.New("daemon unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &presenceDriver{exists: tc.exists, err: tc.err}
			n := NewNodeRuntime(slog.Default(), d, t.TempDir())
			result, err := n.rpcImageExists(context.Background(), json.RawMessage(`{"image_ref":" uncatalogued:v1 "}`))
			if !errors.Is(err, tc.err) {
				t.Fatalf("err=%v", err)
			}
			if d.ref != "uncatalogued:v1" {
				t.Fatalf("ref=%q", d.ref)
			}
			if err == nil && result.(proto.ImageExistsResult).Exists != tc.exists {
				t.Fatalf("result=%+v", result)
			}
		})
	}
	for _, raw := range []string{`{}`, `null`, `{`, `{"image_ref":" "}`} {
		if _, err := (&NodeRuntime{}).rpcImageExists(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestStatusSnapshotDoesNotReportFailedInspectionAsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
		err     error
	}{
		{"present", true, nil}, {"absent", false, nil},
		{"daemon unavailable", false, errors.New("daemon unavailable")},
		{"cancelled", false, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &presenceDriver{exists: tc.present, err: tc.err}
			n := NewNodeRuntime(slog.Default(), d, t.TempDir())
			images, paths := n.StatusSnapshot(context.Background(), []string{"owned:test"})
			if len(paths) == 0 {
				t.Fatal("non-image status lost")
			}
			if tc.err != nil {
				if len(images) != 0 {
					t.Fatalf("inspection failure became an absence: %+v", images)
				}
			} else if len(images) != 1 || images[0].ImageRef != "owned:test" || images[0].Present != tc.present {
				t.Fatalf("valid presence fact lost: %+v", images)
			}
		})
	}
}
