package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
)

// Unlike a presence-only stub, this runtime keeps distinct content identities
// behind the old catalog tag and the reference loaded from the new archive.
type archiveIdentityDriver struct {
	*recording.Driver
	ids        map[string]string
	tagCalls   int
	tagErr     error
	inspectErr error
}

func (d *archiveIdentityDriver) ImageExists(_ context.Context, ref string) (bool, error) {
	return d.ids[ref] != "", nil
}
func (d *archiveIdentityDriver) TagImage(_ context.Context, source, target string) error {
	d.tagCalls++
	if d.tagErr != nil {
		return d.tagErr
	}
	id := d.ids[source]
	if id == "" {
		return errors.New("source image absent")
	}
	d.ids[target] = id
	return nil
}
func (d *archiveIdentityDriver) InspectImage(_ context.Context, ref string) (runtime.ImageInspection, error) {
	if d.inspectErr != nil {
		return runtime.ImageInspection{}, d.inspectErr
	}
	return runtime.ImageInspection{ID: d.ids[ref]}, nil
}
func TestArchiveLoadDoesNotMistakeOldCanonicalForNewContent(t *testing.T) {
	const canonical = "catalog/runner:latest"
	for _, tc := range []struct {
		name, loaded                string
		old                         bool
		loadErr, tagErr, inspectErr error
		wantErr                     string
		wantTags                    int
	}{
		{name: "different reference replaces old canonical", loaded: "publisher/runner:new", old: true, wantTags: 1},
		{name: "tag failure preserves old canonical", loaded: "publisher/runner:new", old: true, tagErr: errors.New("tag denied"), wantErr: "tag denied", wantTags: 1},
		{name: "different reference first load", loaded: "publisher/runner:new", wantTags: 1},
		{name: "same reference replaces old content without retag", loaded: canonical, old: true},
		{name: "load failure preserves old content", loaded: "publisher/runner:new", old: true, loadErr: errors.New("corrupt archive"), wantErr: "corrupt archive"},
		{name: "unknown loaded reference cannot confirm absent canonical", wantErr: "did not report the reference"},
		{name: "metadata inspection failure does not keep stale canonical", loaded: "publisher/runner:new", old: true, inspectErr: errors.New("inspect unavailable"), wantTags: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &archiveIdentityDriver{Driver: recording.New(t.TempDir() + "/spec.json"), ids: map[string]string{}, inspectErr: tc.inspectErr, tagErr: tc.tagErr}
			if tc.old {
				d.ids[canonical] = "old-A"
			}
			status, err := loadImageArchive(context.Background(), d, imageArchiveLoad{CanonicalRef: canonical, RuntimeName: "fixture", Loaded: "loaded", Load: func(context.Context) (string, error) {
				if tc.loadErr != nil {
					return "", tc.loadErr
				}
				if tc.loaded != "" {
					d.ids[tc.loaded] = "new-B"
				}
				return tc.loaded, nil
			}})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v, want %q; status=%+v", err, tc.wantErr, status)
				}
				if status.State == "present" {
					t.Fatalf("unconfirmed content reported present: %+v", status)
				}
				if tc.old && d.ids[canonical] != "old-A" {
					t.Fatal("failed/unknown load changed old canonical")
				}
			} else {
				if err != nil || status.State != "present" || d.ids[canonical] != "new-B" {
					t.Fatalf("status=%+v error=%v canonical=%q", status, err, d.ids[canonical])
				}
				if tc.inspectErr == nil && status.ImageID != "new-B" {
					t.Fatalf("status identity=%q", status.ImageID)
				}
				if tc.inspectErr != nil && status.ImageID != "" {
					t.Fatal("inspection failure invented image identity")
				}
			}
			if d.tagCalls != tc.wantTags {
				t.Fatalf("tag calls=%d, want %d", d.tagCalls, tc.wantTags)
			}
		})
	}
}

func TestURLArchiveRetagsAnExistingCanonicalWhenItCarriedAnotherReference(t *testing.T) {
	h := newArchiveLoadHarness(t)
	const published = "publisher/runner:new"
	h.driver.InjectLoadedRef(published)
	if _, err := h.node.rpcImageLoadURL(context.Background(), h.params(h.size)); err != nil {
		t.Fatal(err)
	}
	status := h.awaitTerminal(t)
	if status.State != "present" {
		t.Fatalf("status=%+v", status)
	}
	want := "TagImage(" + published + "->" + loadURLCanonical + ")"
	if !strings.Contains(strings.Join(h.driver.Calls(), "|"), want) {
		t.Fatalf("old canonical incorrectly bypassed replacement: %v", h.driver.Calls())
	}
	h.assertNoStagedArchive(t)
}
func TestStreamedArchiveRetagsAnExistingCanonicalWhenItCarriedAnotherReference(t *testing.T) {
	h := newStreamLoadHarness(t)
	const published = "publisher/runner:new"
	h.driver.InjectLoadedRef(published)
	if err := h.load(t); err != nil {
		t.Fatal(err)
	}
	if status := h.terminal(t); status.State != "present" {
		t.Fatalf("status=%+v", status)
	}
	want := "TagImage(" + published + "->" + streamLoadCanonical + ")"
	if !strings.Contains(strings.Join(h.driver.Calls(), "|"), want) {
		t.Fatalf("old canonical incorrectly bypassed replacement: %v", h.driver.Calls())
	}
}

func TestArchiveLoadUnknownReferenceKeepsPresentCanonicalCompatibility(t *testing.T) {
	const canonical = "catalog/runner:latest"
	d := &archiveIdentityDriver{Driver: recording.New(t.TempDir() + "/spec.json"), ids: map[string]string{canonical: "old-A"}}
	status, err := loadImageArchive(context.Background(), d, imageArchiveLoad{CanonicalRef: canonical, RuntimeName: "fixture", Loaded: "loaded", Load: func(context.Context) (string, error) { return "", nil }})
	if err != nil || status.State != "present" || d.tagCalls != 0 || d.ids[canonical] != "old-A" {
		t.Fatalf("legacy unknown-ref behavior changed: status=%+v err=%v tags=%d", status, err, d.tagCalls)
	}
	// Compatibility does not prove the unidentified archive replaced the content.
	if status.ImageID != "old-A" {
		t.Fatalf("invented identity for unknown loaded reference: %+v", status)
	}
}
