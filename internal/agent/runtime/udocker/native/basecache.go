package native

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The base rootfs cache keeps one unpacked, never-launched rootfs per image
// manifest under UDOCKER_DIR/base/<digest>, so a later create clones it
// instead of unpacking again. Whether that pays off depends entirely on the
// filesystem (measured on a 19 GB vLLM image, see
// artifacts/work/2026-09-24-runveil/evidence.md):
//
//   - Without reflink (ext4, overlay) a clone is a full copy: slower than
//     unpacking, and it doubles the disk. There is no cache at all.
//   - With reflink (xfs, btrfs) a clone takes seconds instead of minutes. But
//     cloning a freshly unpacked tree forces its dirty pages to disk first,
//     which added two minutes to the create that built it. So the first
//     create unpacks straight into the container and returns; the cache is
//     built afterwards, in the background, from the layers.
type baseCache struct {
	mu       sync.Mutex
	probed   bool
	reflink  bool
	building map[string]bool
	wg       sync.WaitGroup
}

// baseDir is where the cached rootfs of a manifest lives.
func (l Layout) baseDir(manifestID string) string {
	return filepath.Join(l.Dir, "base", strings.TrimPrefix(manifestID, "sha256:"))
}

// reflinkSupported probes UDOCKER_DIR once (Engine.Reflink overrides the
// probe). Without reflink, bases left by earlier versions are removed: they
// only cost disk.
func (e *Engine) reflinkSupported() bool {
	e.base.mu.Lock()
	defer e.base.mu.Unlock()
	if !e.base.probed {
		e.base.probed = true
		probe := e.Reflink
		if probe == nil {
			probe = ReflinkSupported
		}
		e.base.reflink = probe(e.Layout.Dir)
		if !e.base.reflink {
			if err := os.RemoveAll(filepath.Join(e.Layout.Dir, "base")); err != nil {
				slog.Warn("remove unused base rootfs cache", "dir", e.Layout.Dir, "err", err)
			}
		}
	}
	return e.base.reflink
}

// scheduleBase starts building the base of manifestID in the background,
// unless reflink is unavailable or a build is already running.
func (e *Engine) scheduleBase(manifestID, image string, layers []LayerSource, onLine func(string)) {
	if manifestID == "" || !e.reflinkSupported() {
		return
	}
	e.base.mu.Lock()
	if e.base.building == nil {
		e.base.building = map[string]bool{}
	}
	if e.base.building[manifestID] {
		e.base.mu.Unlock()
		return
	}
	e.base.building[manifestID] = true
	e.base.wg.Add(1)
	e.base.mu.Unlock()
	report(onLine, "building a reusable base rootfs in the background")
	go func() {
		defer func() {
			e.base.mu.Lock()
			delete(e.base.building, manifestID)
			e.base.mu.Unlock()
			e.base.wg.Done()
		}()
		if err := e.buildBase(context.Background(), manifestID, image, layers); err != nil {
			slog.Warn("base rootfs cache not built", "image", image, "err", err)
		}
	}()
}

// WaitBackground blocks until background work -- base builds and rootfs
// prepared during a pull -- has finished.
func (e *Engine) WaitBackground() { e.base.wg.Wait() }

// buildBase unpacks layers into base/<digest>.building and publishes it by
// rename, the only way a base ever becomes visible. It gives up when the
// image stops being registered meanwhile, so a removed image cannot leave a
// base behind.
func (e *Engine) buildBase(ctx context.Context, manifestID, image string, layers []LayerSource) error {
	final := e.Layout.baseDir(manifestID)
	if isCompleteBase(filepath.Join(final, "ROOT")) {
		return nil
	}
	building := final + ".building"
	if err := os.RemoveAll(building); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(building)
		}
	}()
	unpacker := &Unpacker{Root: filepath.Join(building, "ROOT"), Workers: e.Unpackers, Owner: e.Layout.Owner}
	if err := unpacker.Unpack(ctx, layers); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(building, ".complete"), []byte("ok\n"), 0o644); err != nil {
		return err
	}
	if id, err := e.Layout.ImageID(ParseRef(image)); err != nil || id != manifestID {
		return fmt.Errorf("image %s was removed while its base was being built", image)
	}
	if err := os.RemoveAll(final); err != nil {
		return err
	}
	if err := os.Rename(building, final); err != nil {
		return err
	}
	published = true
	return nil
}
