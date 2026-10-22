// Package imagepull is how a node pulls an image from an ordered list of
// sources: it ranks the sources it can choose between, logs in where a registry
// needs it, gives up on a source that stops downloading, remembers which
// registries just failed, and parses the runtime's progress output.
package imagepull

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

var pullBytesRE = regexp.MustCompile(`(?i)([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb)\s*/\s*([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb)`)
var pullLayerRE = regexp.MustCompile(`^\s*(?:sha256:)?([0-9a-f]{8,64}):`)

// Progress aggregates one pull's output.
type Progress struct {
	mu       sync.Mutex
	sawBytes bool
	// unreadable: the driver said it no longer understands the runtime's
	// output (runtime.PullLineUnreadable); the pull is not judged again.
	unreadable bool
	layers     map[string]pullLayerProgress
	lastBytes  int64
	lastAt     time.Time
	lastRate   int64
	lastSent   time.Time
	phase      string
	lastLine   string
}

const imagePullRateStaleAfter = 5 * time.Second

// pullGlobalLayer collects the lines that name no layer.
const pullGlobalLayer = "_global"

// imagePullReportInterval is the shortest gap between two released lines of the
// same phase. It matches the agent's egress limiter, so a released line is not
// then dropped again downstream.
const imagePullReportInterval = time.Second

type pullLayerProgress struct {
	done  int64
	total int64
	// step is how far done moves at a time in the runtime's display
	// (runtime.PullLineDownloadingInSteps); zero for an exact count.
	step  int64
	phase string
}

func NewProgress() *Progress {
	return &Progress{layers: map[string]pullLayerProgress{}}
}

// Observe turns the human-readable docker pull stream into an aggregate
// progress snapshot. Runtimes such as udocker may not expose byte counters;
// their phase and last line are still useful to the operator.
func (p *Progress) Observe(line string, now time.Time) (phase string, done, total, rate int64, progress float64, changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	line = strings.TrimSpace(line)
	p.lastLine = line
	if runtime.IsPullLineUnreadable(line) {
		p.unreadable = true
	}
	oldPhase := p.phase
	layer := pullGlobalLayer
	if m := pullLayerRE.FindStringSubmatch(line); len(m) == 2 {
		layer = m[1]
	}
	l := p.layers[layer]
	lower := strings.ToLower(line)
	if strings.Contains(lower, "extracting") {
		p.phase = proto.PhaseImageExtract
		l.phase = "extracting"
	} else if strings.Contains(lower, "downloading") {
		p.phase = proto.PhaseImagePull
		l.phase = "downloading"
	} else if strings.Contains(lower, "pull complete") || strings.Contains(lower, "download complete") || strings.Contains(lower, "already exists") {
		l.phase = "complete"
	} else if p.phase == "" {
		p.phase = proto.PhaseImagePull
	}
	if m := pullBytesRE.FindStringSubmatch(line); len(m) == 5 {
		l.done = parsePullBytes(m[1], m[2])
		l.total = parsePullBytes(m[3], m[4])
		l.step = runtime.PullLineStep(line)
	}
	if strings.Contains(lower, "pull complete") || strings.Contains(lower, "download complete") || strings.Contains(lower, "already exists") {
		if l.total > 0 {
			l.done = l.total
		}
	}
	p.layers[layer] = l
	for _, item := range p.layers {
		done += item.done
		total += item.total
	}
	// Only a layer's own counter can arm the stall watch. A count on a line
	// naming no layer is shown but never judged: nothing later can finish it,
	// so it would freeze the watch's view of the pull (a nerdctl row for a
	// blob under 1 KiB once did exactly that).
	if layer != pullGlobalLayer && l.total > 0 {
		p.sawBytes = true
	}
	if total > 0 {
		progress = float64(done) / float64(total)
		if progress > 1 {
			progress = 1
		}
	}
	if p.lastAt.IsZero() {
		p.lastBytes, p.lastAt = done, now
	} else if done < p.lastBytes {
		// A failed prefetch may restart the same layer through udocker. Reset
		// the rate baseline instead of exposing a negative transfer rate.
		p.lastBytes, p.lastAt, p.lastRate = done, now, 0
	} else if done != p.lastBytes {
		if elapsed := now.Sub(p.lastAt); elapsed >= 250*time.Millisecond {
			p.lastRate = int64(float64(done-p.lastBytes) / elapsed.Seconds())
			p.lastBytes, p.lastAt = done, now
		}
	}
	rate = p.lastRate
	// A pull stream emits thousands of lines per phase. Only a phase change or
	// the elapsed interval releases one, and lastSent advances solely when a
	// line is released: advancing it unconditionally would make every later
	// line "recent" and suppress the stream entirely.
	changed = now.Sub(p.lastSent) >= imagePullReportInterval || p.phase != oldPhase
	if changed {
		p.lastSent = now
	}
	return p.phase, done, total, rate, progress, changed
}

// Snapshot keeps a quiet or stalled pull observable even when the runtime
// emits no new line. The byte counters remain useful, but a rate derived from
// old bytes must expire instead of looking like current throughput.
func (p *Progress) Snapshot(now time.Time) (message, phase string, done, total, rate int64, progress float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, item := range p.layers {
		done += item.done
		total += item.total
	}
	if total > 0 {
		progress = float64(done) / float64(total)
		if progress > 1 {
			progress = 1
		}
	}
	rate = p.lastRate
	if !p.lastAt.IsZero() && now.Sub(p.lastAt) >= imagePullRateStaleAfter {
		rate = 0
		p.lastRate = 0
	}
	return p.lastLine, p.phase, done, total, rate, progress
}

func parsePullBytes(value, unit string) int64 {
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	switch strings.ToLower(unit) {
	case "gb":
		v *= 1024 * 1024 * 1024
	case "mb":
		v *= 1024 * 1024
	case "kb":
		v *= 1024
	}
	return int64(v)
}

// Fingerprint is what advances while a pull makes headway: the phase, the
// bytes received and the layers finished. A pull whose fingerprint stays the
// same is not making headway.
type Fingerprint struct {
	Phase     string
	Done      int64
	Completed int
}

// Fingerprint reports the pull's headway marker. downloading is true only when
// a stall can be told from the runtime's output: per-layer byte counters have
// been seen (a runtime that prints none, like docker without a terminal, is
// silent for the whole of a large layer and proves nothing by it), the pull is
// not extracting, and some bytes announced for a layer are still outstanding
// (after the last byte the runtime may legitimately spend minutes unpacking and
// registering the image). Lines that name no layer do not count, and nothing
// is judged once the driver has said it no longer understands the output.
func (p *Progress) Fingerprint() (fp Fingerprint, downloading bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var done, total int64
	for key, l := range p.layers {
		if key == pullGlobalLayer {
			continue
		}
		done += l.done
		total += l.total
		if l.phase == "complete" || (l.total > 0 && l.done >= l.total) {
			fp.Completed++
		}
	}
	fp.Phase, fp.Done = p.phase, done
	downloading = p.sawBytes && !p.unreadable && p.phase != proto.PhaseImageExtract && !(total > 0 && done >= total)
	return fp, downloading
}

// StallWindow is how long the pull may show no headway before it counts as
// stalled: base, or longer when a layer still downloading is shown only in
// coarse steps, so that one step at minRate bytes per second fits in it. A
// download slower than minRate on such a layer is let run (CandidateTimeout
// still bounds it); a faster one would have moved a step within the window.
func (p *Progress) StallWindow(base time.Duration, minRate int64) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	window := base
	if minRate <= 0 {
		return window
	}
	for key, l := range p.layers {
		if key == pullGlobalLayer || l.step <= 0 || l.phase == "complete" || (l.total > 0 && l.done >= l.total) {
			continue
		}
		if w := time.Duration(float64(l.step) / float64(minRate) * float64(time.Second)).Round(time.Second); w > window {
			window = w
		}
	}
	return window
}
