package nerdctl

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The converter is written against nerdctl 2.1.6's table. A later nerdctl may
// print it differently; whatever it prints, the stall watch must never be left
// judging a count the converter has stopped following. These cases take the
// real 2.1.6 output, change one thing about its format, and replay it through
// the converter into the node's progress parser.

// pullMutations each rewrite the real table the way a nerdctl release might.
var pullMutations = []struct {
	name   string
	mutate func(string) string
}{
	{"a finished blob says finished, not done", func(s string) string {
		return strings.ReplaceAll(s, ":    done    ", ":    finished")
	}},
	{"a downloading blob says fetching", func(s string) string {
		return strings.ReplaceAll(s, "downloading", "fetching   ")
	}},
	{"a queued blob says queued, not waiting", func(s string) string {
		return strings.ReplaceAll(s, "waiting", "queued ")
	}},
	{"unpacking gets a status of its own", func(s string) string {
		// Only the frames after the download carry it: an all-done row
		// becomes unpacking.
		return strings.ReplaceAll(s, ":    done    ", ":    unpacking")
	}},
	{"byte counts lose their units", func(s string) string {
		return regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?) ?[KMGT]?i?B?/([0-9]+(?:\.[0-9]+)?) ?[KMGT]?i?B`).ReplaceAllString(s, "$1/$2")
	}},
	{"byte counts become a percentage", func(s string) string {
		return regexp.MustCompile(`[0-9.]+ ?[KMGT]?i?B?/[0-9.]+ ?[KMGT]?i?B`).ReplaceAllString(s, "42%")
	}},
	{"blob keys lose their kind prefix", func(s string) string {
		return regexp.MustCompile(`(?m)^(layer|config|manifest|index)-sha256:`).ReplaceAllString(s, "sha256:")
	}},
	{"blob keys use another digest", func(s string) string {
		return strings.ReplaceAll(s, "-sha256:", "-sha512:")
	}},
}

func feedPull(p *imagepull.Progress, now time.Time, chunks ...string) (lines []string) {
	f := newPullFrames(func(s string) {
		lines = append(lines, s)
		p.Observe(s, now)
	})
	for _, chunk := range chunks {
		for _, l := range strings.Split(strings.TrimSuffix(chunk, "\n"), "\n") {
			f.line(l)
		}
	}
	return lines
}

// The real table is understood as it is: it never turns the stall check off.
func TestPullFramesUnderstandTheRealTable(t *testing.T) {
	unpack := readPullFixture(t, "unpacking-frame.txt")
	lines := feedPull(imagepull.NewProgress(), time.Unix(100, 0),
		readPullFixture(t, "download.txt"), unpack, unpack, readPullFixture(t, "end.txt"))
	for _, l := range lines {
		if runtime.IsPullLineUnreadable(l) {
			t.Fatalf("the real nerdctl 2.1.6 table was declared unreadable: %q", l)
		}
	}
}

// Whatever changed, once the download is over and nerdctl redraws the same
// frame for the unpack, nothing is left armed: either the converter still
// reads the table, or it says it cannot and the watch is off.
func TestPullFramesNeverLeaveAnUnreadTableJudged(t *testing.T) {
	download := readPullFixture(t, "download.txt")
	unpack := readPullFixture(t, "unpacking-frame.txt")
	for _, m := range pullMutations {
		t.Run(m.name, func(t *testing.T) {
			p := imagepull.NewProgress()
			lines := feedPull(p, time.Unix(100, 0), m.mutate(download), m.mutate(unpack), m.mutate(unpack))
			if _, downloading := p.Fingerprint(); downloading {
				t.Fatalf("an unpacking pull is still judged after the table changed; lines:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// Mid-download, nerdctl's output changes: a count read from the old format
// must not stay armed once the converter can no longer follow it.
func TestPullFramesNeverJudgeAFrozenCountMidDownload(t *testing.T) {
	frame := readPullFixture(t, "downloading-frame.txt")
	convert := func(s string) string {
		return strings.Join(feedPull(imagepull.NewProgress(), time.Unix(100, 0), s), "\n")
	}
	for _, m := range pullMutations {
		t.Run(m.name, func(t *testing.T) {
			changed := m.mutate(frame)
			if convert(changed) == convert(frame) {
				t.Skip("this change leaves the mid-download frame as it was")
			}
			p := imagepull.NewProgress()
			feedPull(p, time.Unix(100, 0), frame)
			if _, armed := p.Fingerprint(); !armed {
				t.Fatal("the real frame did not arm the stall watch")
			}
			f := newPullFrames(func(s string) { p.Observe(s, time.Unix(101, 0)) })
			for _, l := range strings.Split(strings.TrimSuffix(changed+changed, "\n"), "\n") {
				f.line(l)
			}
			if _, downloading := p.Fingerprint(); downloading {
				t.Fatal("the watch stays armed on counts the changed table no longer updates")
			}
		})
	}
}

// End to end: a nerdctl that renamed "done" downloads, then unpacks for far
// longer than the stall timeout. The pull must succeed.
func TestWalkLetsAnUnreadableNerdctlPullFinish(t *testing.T) {
	shortStallWatch(t)
	rename := pullMutations[0].mutate
	installFakeNerdctl(t, frameScript(t,
		frameStep{rename(readPullFixture(t, "download.txt")), 1},
		frameStep{rename(readPullFixture(t, "unpacking-frame.txt")), 60},
		frameStep{rename(readPullFixture(t, "end.txt")), 1},
	))
	var unreadable bool
	_, err := imagepull.New(imagepull.Config{}).Walk(context.Background(), New(),
		[]proto.ImagePullCandidate{{Ref: "localhost:5098/t73/raw:2"}}, imagepull.Options{}, imagepull.Hooks{
			Line: func(_, line string, _ imagepull.Snapshot, _ bool) {
				if runtime.IsPullLineUnreadable(line) {
					unreadable = true
				}
			},
		})
	if err != nil {
		t.Fatalf("a pull whose table changed was given up on: %v", err)
	}
	if !unreadable {
		t.Fatal("the changed table was not reported as unreadable")
	}
}
