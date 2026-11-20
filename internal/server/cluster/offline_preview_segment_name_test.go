//go:build linux && dqlite

package cluster

import "testing"

func TestOfflineSegmentNamesCannotEvadeNativeByteGuard(t *testing.T) {
	requireDatabaseHelper(t)
	for _, name := range []string{"-1-2", "1--2", "+1-2", " 1-2", "1-2", "0000000000000000-0000000000000002", "0000000000000002-0000000000000001", "open--1", "open-01", "snapshot-1-2-3.meta.extra"} {
		t.Run(name, func(t *testing.T) {
			if kind, err := offlinePreviewSegmentKind(name); err == nil {
				t.Fatalf("noncanonical native filename escaped guard: %q kind%q", name, kind)
			}
		})
	}
	for name, want := range map[string]string{
		"0000000000000001-0000000000000002": "closed", "open-1": "open",
		"snapshot-1-2-3": "snapshot", "snapshot-1-2-3.meta": "snapshot",
		"info.yaml": "", "cluster.yaml": "", "metadata1": "", "dqlite-lock": "", "0-temp": "", " 1-temp": "",
		"-99999999999999999999999-2": "", "10000000000000000-10000000000000001": "", "1-2suffix": "",
	} {
		t.Run(name, func(t *testing.T) {
			if kind, err := offlinePreviewSegmentKind(name); err != nil || kind != want {
				t.Fatalf("kind%q want%q err%v", kind, want, err)
			}
		})
	}
}
