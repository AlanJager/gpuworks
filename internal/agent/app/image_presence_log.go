package app

import (
	"context"
	"log/slog"
	"sync"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// imagePresenceLog remembers the inventory's last answer per image so the
// agent logs what changed, not the same answer every minute: an image found
// (the first time, or after it was gone) or one that disappeared. A present
// image is logged with the identity the runtime reports for it - the evidence
// that tells a real image from a runtime answering "exists" for anything.
type imagePresenceLog struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (l *imagePresenceLog) observe(ctx context.Context, log *slog.Logger, d runtime.Driver, ref string, present bool) {
	l.mu.Lock()
	before, known := l.seen[ref]
	if l.seen == nil {
		l.seen = map[string]bool{}
	}
	l.seen[ref] = present
	l.mu.Unlock()
	switch {
	case known && before == present:
		return
	case !present && !known:
		// An image never seen is the ordinary starting point, not news.
		return
	case !present:
		log.Info("image no longer present", "image", ref, "runtime", d.Name())
		return
	}
	attrs := []any{"image", ref, "runtime", d.Name()}
	if inspector, ok := d.(runtime.ImageInspector); ok {
		if inspected, err := inspector.InspectImage(ctx, ref); err == nil {
			attrs = append(attrs, "image_id", inspected.ID)
		} else {
			attrs = append(attrs, "inspect_err", err)
		}
	}
	log.Info("image present", attrs...)
}

// forget drops refs no longer inventoried, so the map follows the catalog.
func (l *imagePresenceLog) forget(keep []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.seen) == 0 {
		return
	}
	wanted := make(map[string]bool, len(keep))
	for _, ref := range keep {
		wanted[ref] = true
	}
	for ref := range l.seen {
		if !wanted[ref] {
			delete(l.seen, ref)
		}
	}
}
