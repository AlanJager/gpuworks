package main

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
)

func TestExplainFullDiskSaysWhenTheDataDirectoryIsWhyTheServerStopped(t *testing.T) {
	saved := filesystemUsage
	t.Cleanup(func() { filesystemUsage = saved })
	ioErr := errors.New(`pragma "PRAGMA journal_mode=WAL;": disk I/O error (4874)`)
	portErr := errors.New("listen tcp :8800: bind: address already in use")
	cases := []struct {
		name         string
		err          error
		total, avail int64
		statErr      error
		want         string // substring; "" = the error unchanged
	}{
		{"opening the WAL on a full disk", ioErr, 100 << 30, 0, nil, "has 0 B free of 100.0 GiB"},
		{"a few KiB left is still full", ioErr, 64 << 20, 40 << 10, nil, "has 40.0 KiB free of 64.0 MiB"},
		{"ENOSPC with room again by now", fmt.Errorf("write key: %w", syscall.ENOSPC), 100 << 30, 5 << 30, nil, "has 5.0 GiB free"},
		{"another reason, room to spare", portErr, 100 << 30, 50 << 30, nil, ""},
		{"statfs unavailable", ioErr, 0, 0, errors.New("no statfs"), ""},
	}
	for _, tc := range cases {
		filesystemUsage = func(string) (int64, int64, error) { return tc.total, tc.avail, tc.statErr }
		got := explainFullDisk(tc.err, "/opt/gpuworks/data")
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: %v lost the original error", tc.name, got)
		}
		if tc.want == "" {
			if got != tc.err {
				t.Errorf("%s: %v, want the error unchanged", tc.name, got)
			}
			continue
		}
		if !strings.Contains(got.Error(), tc.want) || !strings.Contains(got.Error(), "/opt/gpuworks/data") || !strings.HasPrefix(got.Error(), tc.err.Error()) {
			t.Errorf("%s: %q, want the original error then %q and the directory", tc.name, got, tc.want)
		}
	}
	if explainFullDisk(nil, "/x") != nil {
		t.Error("nil error was explained")
	}
}
