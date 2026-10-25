package nerdctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/proto"
)

// These run the node's real pull path — imagepull's stall watch over this
// driver — against a fake nerdctl that replays the captured output (see
// readPullFixture), with the watch shrunk from minutes to milliseconds.

func shortStallWatch(t *testing.T) {
	t.Helper()
	oldStall, oldBeat, oldRate := imagepull.StallTimeout, imagepull.HeartbeatInterval, imagepull.StallMinRate
	// 3 min -> 150 ms is 1200 times faster; the minimum rate scales with it,
	// so a 0.1 GiB step stretches the window to ~0.9 s (rounded to 1 s).
	imagepull.StallTimeout, imagepull.HeartbeatInterval = 150*time.Millisecond, 5*time.Millisecond
	imagepull.StallMinRate = oldRate * 1200
	t.Cleanup(func() {
		imagepull.StallTimeout, imagepull.HeartbeatInterval, imagepull.StallMinRate = oldStall, oldBeat, oldRate
	})
}

// fixtureScript writes the fixtures where a fake nerdctl can cat them and
// returns a shell fragment that replays: first, then repeat (n times, 10 ms
// apart), then last.
func fixtureScript(t *testing.T, first, repeat string, n int, last string) string {
	t.Helper()
	dir := t.TempDir()
	path := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(readPullFixture(t, name)), 0o600); err != nil {
			t.Fatal(err)
		}
		return "'" + p + "'"
	}
	script := ""
	if first != "" {
		script += "cat " + path(first) + " >&2\n"
	}
	script += "r=" + path(repeat) + "\ni=0\nwhile [ $i -lt " + strconv.Itoa(n) + " ]; do cat \"$r\" >&2; sleep 0.01; i=$((i+1)); done\n"
	if last != "" {
		script += "cat " + path(last) + " >&2\n"
	}
	return script
}

type phaseLog struct {
	mu     sync.Mutex
	phases []string
	total  int64
	last   string
}

func (l *phaseLog) hooks() imagepull.Hooks {
	return imagepull.Hooks{Line: func(_, _ string, s imagepull.Snapshot, _ bool) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if n := len(l.phases); n == 0 || l.phases[n-1] != s.Phase {
			l.phases = append(l.phases, s.Phase)
		}
		if s.Total > l.total {
			l.total = s.Total
		}
		l.last = s.Message
	}}
}

// The download finishes, then nerdctl spends far longer than the stall
// timeout unpacking, redrawing the same all-done frame: the pull must succeed
// and be reported as extracting.
func TestWalkLetsANerdctlPullUnpackPastTheStallTimeout(t *testing.T) {
	shortStallWatch(t)
	installFakeNerdctl(t, fixtureScript(t, "download.txt", "unpacking-frame.txt", 60, "end.txt"))
	var log phaseLog
	res, err := imagepull.New(imagepull.Config{}).Walk(context.Background(), New(),
		[]proto.ImagePullCandidate{{Ref: "localhost:5098/t73/raw:2"}}, imagepull.Options{}, log.hooks())
	if err != nil {
		t.Fatalf("an unpacking pull was given up on: %v", err)
	}
	if res.Ref != "localhost:5098/t73/raw:2" {
		t.Fatalf("result = %+v", res)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if strings.Join(log.phases, ",") != proto.PhaseImagePull+","+proto.PhaseImageExtract {
		t.Fatalf("phases = %v, want image_pull then image_extract", log.phases)
	}
	if log.total < 2<<30 {
		t.Fatalf("largest reported total = %d, want the image's ~2.4 GiB", log.total)
	}
	if !strings.HasPrefix(log.last, "Extracting downloaded layers") {
		t.Fatalf("last message = %q", log.last)
	}
}

// A download whose bytes stop moving is given up on, even though nerdctl
// keeps redrawing its table (and its elapsed time) every 100 ms.
func TestWalkGivesUpOnANerdctlDownloadThatStops(t *testing.T) {
	shortStallWatch(t)
	installFakeNerdctl(t, fixtureScript(t, "", "downloading-frame.txt", 150, ""))
	_, err := imagepull.New(imagepull.Config{}).Walk(context.Background(), New(),
		[]proto.ImagePullCandidate{{Ref: "localhost:5098/t73/raw:2"}}, imagepull.Options{}, imagepull.Hooks{})
	var all *imagepull.AllFailedError
	if !errors.As(err, &all) || !strings.Contains(err.Error(), "no download progress") {
		t.Fatalf("a stopped download was not given up on: %v", err)
	}
}

// coarseFrame is a generated frame (jobs.Display's format, as in
// TestPullFramesReadTruncatedUnits) with the 1.5 GiB layer at done, the only
// blob of the image.
func coarseFrame(status, done string) string {
	row := "layer-sha256:" + bigLayer + ":    " + status + "    |\x1b[32m++++\x1b[0m----| "
	if done != "" {
		row += done + "/1.5 GiB "
	}
	return row + "\nelapsed: 9.0 s    total:  1.1 Gi (1 MiB/s)\n"
}

// frameScript writes each frame to a file and replays it n times, 10 ms apart.
func frameScript(t *testing.T, steps ...frameStep) string {
	t.Helper()
	dir := t.TempDir()
	script := ""
	for i, s := range steps {
		p := filepath.Join(dir, strconv.Itoa(i))
		if err := os.WriteFile(p, []byte(s.frame), 0o600); err != nil {
			t.Fatal(err)
		}
		script += "i=0\nwhile [ $i -lt " + strconv.Itoa(s.n) + " ]; do cat '" + p + "' >&2; sleep 0.01; i=$((i+1)); done\n"
	}
	return script
}

// frameStep is one frame of a generated replay and how many times it is drawn.
type frameStep struct {
	frame string
	n     int
}

// Past 1 GiB nerdctl shows a layer's bytes in 0.1 GiB steps; a download that
// takes longer than the stall timeout to cover one step is slow, not stopped,
// and must not be given up on.
func TestWalkWaitsOutASlowNerdctlDownloadShownInCoarseSteps(t *testing.T) {
	shortStallWatch(t)
	// Each step outlasts the plain 150 ms stall timeout (25 frames, 10 ms plus
	// a process start apiece) yet stays far inside the stretched 1 s window,
	// which a loaded runner slows the replay toward.
	installFakeNerdctl(t, frameScript(t,
		frameStep{coarseFrame("downloading", "  1.1 GiB"), 25},
		frameStep{coarseFrame("downloading", "  1.2 GiB"), 25},
		frameStep{coarseFrame("downloading", "  1.3 GiB"), 25},
		frameStep{coarseFrame("done       ", ""), 3},
	))
	_, err := imagepull.New(imagepull.Config{}).Walk(context.Background(), New(),
		[]proto.ImagePullCandidate{{Ref: "localhost:5098/t73/raw:2"}}, imagepull.Options{}, imagepull.Hooks{})
	if err != nil {
		t.Fatalf("a slow download in coarse steps was given up on: %v", err)
	}
}

// The stretched window still ends: a coarse count that never moves is a stall.
func TestWalkGivesUpOnACoarseNerdctlDownloadThatStops(t *testing.T) {
	shortStallWatch(t)
	installFakeNerdctl(t, frameScript(t, frameStep{coarseFrame("downloading", "  1.1 GiB"), 300}))
	_, err := imagepull.New(imagepull.Config{}).Walk(context.Background(), New(),
		[]proto.ImagePullCandidate{{Ref: "localhost:5098/t73/raw:2"}}, imagepull.Options{}, imagepull.Hooks{})
	if err == nil || !strings.Contains(err.Error(), "no download progress for 1s") {
		t.Fatalf("a stopped coarse download was not given up on after the stretched window: %v", err)
	}
}
