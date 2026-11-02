package native

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func entryModeLayer(t *testing.T, entries []entry) LayerSource {
	t.Helper()
	data := buildLayer(t, true, entries)
	return LayerSource{Digest: digestFor(0), Size: int64(len(data)), Open: func(context.Context) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(data)), nil
	}}
}

func TestUnpackSpecialRegularModesDoNotReachRestrictedCreate(t *testing.T) {
	for _, test := range []struct {
		name    string
		tarMode int64
		special os.FileMode
	}{
		{"ordinary-suid", 0o4755, os.ModeSetuid},
		{"usr/bin/pkexec", 0o4755, os.ModeSetuid},
		{"ordinary-sgid", 0o2755, os.ModeSetgid},
		{"ordinary-sticky", 0o1755, os.ModeSticky},
		{"ordinary-both-setid", 0o6755, os.ModeSetuid | os.ModeSetgid},
		{"ordinary-executable", 0o755, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := openRegularFile
			t.Cleanup(func() { openRegularFile = original })
			var createModes []os.FileMode
			openRegularFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
				createModes = append(createModes, mode)
				// Model the measured sandbox boundary: reject special mode bits
				// only at creation; permitted opens perform real filesystem I/O.
				if mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
					return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EPERM}
				}
				return os.OpenFile(path, flags, mode)
			}
			body := "complete executable bytes\x00\x01\xff; filename must not determine policy\n"
			unpacker := Unpacker{Root: filepath.Join(t.TempDir(), "ROOT"), Workers: 1,
				Owner: &Owner{UID: os.Geteuid(), GID: os.Getegid()}}
			layer := entryModeLayer(t, []entry{
				{name: "tmp", dir: true, mode: 0o1777},
				{name: test.name, mode: test.tarMode, body: body},
			})
			if err := unpacker.Unpack(context.Background(), []LayerSource{layer}); err != nil {
				t.Fatalf("ordinary creation of setid image entry failed: %v", err)
			}
			if len(createModes) != 1 || createModes[0]&^os.ModePerm != 0 {
				t.Fatalf("create mode contains special bits: %v", createModes)
			}
			path := filepath.Join(unpacker.Root, test.name)
			if data, err := os.ReadFile(path); err != nil || string(data) != body {
				t.Fatalf("entry was skipped or truncated: %q/%v", data, err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := info.Mode().Perm(), os.FileMode(0o755)&^os.FileMode(unpacker.umask); got != want || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != test.special {
				t.Fatalf("final normal-host mode %v did not preserve umask/special flags %v", info.Mode(), test.special)
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid() {
				t.Fatalf("unpacking identity changed: %+v", info.Sys())
			}
			dir, err := os.Stat(filepath.Join(unpacker.Root, "tmp"))
			if err != nil || !dir.IsDir() || dir.Mode()&os.ModeSticky == 0 || dir.Mode().Perm() != os.FileMode(0o777)&^os.FileMode(unpacker.umask) {
				t.Fatalf("deferred sticky directory metadata regressed: %v/%v", dir, err)
			}
		})
	}
}

func TestUnpackRegularOpenPermissionFailureIsNotSkipped(t *testing.T) {
	original := openRegularFile
	t.Cleanup(func() { openRegularFile = original })
	openRegularFile = func(path string, flags int, mode os.FileMode) (*os.File, error) {
		if filepath.Base(path) == "denied" {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}
		return os.OpenFile(path, flags, mode)
	}
	unpacker := Unpacker{Root: filepath.Join(t.TempDir(), "ROOT"), Workers: 1}
	layer := entryModeLayer(t, []entry{
		{name: "written", body: "first file contents"},
		{name: "denied", body: "must not skip denied entry", mode: 0o4755},
		{name: "after-error", body: "must not continue after open failed"},
	})
	if err := unpacker.Unpack(context.Background(), []LayerSource{layer}); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("genuine open permission failure was swallowed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(unpacker.Root, "written")); err != nil || string(data) != "first file contents" {
		t.Fatalf("partial failure lost previous successful entry: %q/%v", data, err)
	}
	for _, name := range []string{"denied", "after-error"} {
		if _, err := os.Stat(filepath.Join(unpacker.Root, name)); !os.IsNotExist(err) {
			t.Fatalf("failed entry was skipped/continued or materialized: %s/%v", name, err)
		}
	}
}
