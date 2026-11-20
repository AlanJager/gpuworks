//go:build linux

package packages

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func TestPackageRootProgressFollowsDurableOwnedFiles(t *testing.T) {
	for _, mode := range []string{"complete", "callback-error", "cancel", "corrupt-file", "replace-stage"} {
		t.Run(mode, func(t *testing.T) {
			root, rootPath := packageRootFixture(t)
			source := durablePackageFixture(t)
			expected, err := inferencecatalog.PackageDigest(source)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			callbackErr := errors.New("completed progress write failed")
			calls := 0
			digest, published, err := PutRootContextWithProgress(ctx, root, source, func() error {
				calls++
				if mode == "corrupt-file" && calls > 1 {
					return nil
				}
				if _, e := root.Lstat(expected); !errors.Is(e, os.ErrNotExist) {
					t.Error("progress acknowledged an unpublished package")
				}
				stage := packageRootStage(t, root)
				if stage == "" {
					t.Error("progress preceded a captured stage")
					return callbackErr
				}
				var copied string
				e := fs.WalkDir(root.FS(), stage, func(name string, entry fs.DirEntry, e error) error {
					if e != nil {
						return e
					}
					if entry.IsDir() {
						return nil
					}
					rel := strings.TrimPrefix(name, stage+"/")
					want, e := fs.ReadFile(source, rel)
					if e != nil {
						return e
					}
					got, e := root.ReadFile(name)
					if e != nil {
						return e
					}
					if string(got) != string(want) {
						return errors.New("progress preceded complete file bytes")
					}
					copied = name
					return nil
				})
				if e != nil || copied == "" {
					t.Errorf("owned copy not complete before progress: %v", e)
					return callbackErr
				}
				if calls == 1 {
					switch mode {
					case "callback-error":
						return callbackErr
					case "cancel":
						cancel()
					case "corrupt-file":
						return root.WriteFile(copied, []byte("changed after completed copy"), 0644)
					case "replace-stage":
						if e := os.Rename(filepath.Join(rootPath, stage), filepath.Join(rootPath, stage+"-held")); e != nil {
							return e
						}
						return root.Mkdir(stage, 0755)
					}
				}
				return nil
			})
			if mode == "complete" {
				count := 0
				_ = fs.WalkDir(source, ".", func(_ string, e fs.DirEntry, err error) error {
					if err == nil && !e.IsDir() {
						count++
					}
					return err
				})
				if err != nil || !published || digest != expected || calls != count {
					t.Fatalf("completed publication/count changed: published=%v err=%v calls=%d want=%d", published, err, calls, count)
				}
				if _, e := LoadRootContext(t.Context(), root, digest); e != nil {
					t.Fatal(e)
				}
				if _, again, e := PutRootContext(t.Context(), root, source); e != nil || again {
					t.Fatalf("legacy noop changed: %v/%v", again, e)
				}
			} else {
				if err == nil || published || calls == 0 {
					t.Fatalf("progress bypassed final ownership/bytes/cancellation: err=%v published=%v calls=%d", err, published, calls)
				}
				if _, e := root.Lstat(expected); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("failed progress published a digest", e)
				}
				if mode == "corrupt-file" && errors.Is(err, callbackErr) {
					t.Fatal("test callback hid production corruption validation", err)
				}
				if mode == "callback-error" && !errors.Is(err, callbackErr) {
					t.Fatal("callback cause lost", err)
				}
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal("cancel cause lost", err)
				}
				if (mode == "cancel" || mode == "callback-error") && calls != 1 {
					t.Fatal("stopped work continued progress", calls)
				}
			}
		})
	}
}
