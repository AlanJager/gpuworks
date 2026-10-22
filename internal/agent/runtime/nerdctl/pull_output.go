package nerdctl

import (
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// nerdctl pull redraws a progress table every 100 ms (nerdctl
// pkg/imgutil/jobs.ShowProgress): one row for the reference, one per blob,
// then an "elapsed:" row. Only a downloading row carries bytes, in IEC units
// whose done half is cut to eight characters ("123.4 Mi/3.4 GiB"); finished
// rows say "done" or "exists" with no bytes, and unpacking prints no row of its
// own — the same all-done frame repeats until the layers are unpacked.
//
// pullFrames turns that table into the shared pull lines (runtime.PullLine*):
// a byte count per blob whose bytes changed, carrying the step its display
// moves in (0.1 of the unit, so 0.1 GiB once past 1 GiB), "Download complete"
// or "Already exists" once a blob is finished, and an extracting line while no
// blob is downloading or queued but the pull goes on. Repeated frames produce
// nothing. Rows outside the table — nerdctl's own log and error lines — pass
// through unchanged.
//
// The raw table cannot go to the parser as is: it reads none of the IEC byte
// counts, and the one row it does read — a blob under 1 KiB, "0.0 B/182.0 B" —
// has no key it knows, so a later "done" never finishes it and the stall watch
// fires minutes later on a pull that is downloading or unpacking
// (artifacts/archive/2026-10-20-t73-nerdctl-pull-progress/plan.md).
type pullFrames struct {
	emit func(string)
	// last is the line last emitted for each blob, so an unchanged blob is
	// not repeated every 100 ms.
	last map[string]string
	// downloaded marks the blobs seen downloading.
	downloaded map[string]bool
	// The frame being read: whether it listed a layer, and whether every blob
	// row in it was finished.
	frameLayer   bool
	frameAllDone bool
	frameRows    int
	// unpacking: the previous frame was all finished. unpackSince is the
	// nerdctl elapsed time at which that began; reported is the last whole
	// unpack second announced.
	unpacking   bool
	unpackSince float64
	reported    int
	// unreadable: a row was not understood and the pull was declared
	// unjudgeable (runtime.PullLineUnreadable), once.
	unreadable bool
}

// pullUnpackReportEvery is how often, in seconds of unpacking, the Extracting
// line is repeated with its elapsed time. The line carries no bytes and does
// not feed the stall watch; it only keeps the operator's message current.
const pullUnpackReportEvery = 10

var (
	pullControlRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// pullRowRE is a table row: a key, a status word and the progress bar.
	// Any word is taken, so that a status this code does not know is seen as
	// a table row it cannot read rather than passed on as a message.
	pullRowRE = regexp.MustCompile(`^(\S+):\s+([a-z][a-z-]*)\s+\|`)
	// pullBlobKeyRE is a blob row's key (containerd remotes.MakeRefKey).
	pullBlobKeyRE = regexp.MustCompile(`^([a-z]+)-sha256:([0-9a-f]{64})$`)
	// pullRowBytesRE reads "%8.8s/%s" after the bar: the done half may have
	// lost the end of its unit to the eight-character cut.
	pullRowBytesRE = regexp.MustCompile(`\|\s*([0-9]+(?:\.[0-9]+)?)\s*([KMGTPEZY]?)i?B?/([0-9]+(?:\.[0-9]+)?)\s*([KMGTPEZY]?)i?B\s*$`)
	pullElapsedRE  = regexp.MustCompile(`^elapsed:\s*([0-9]+(?:\.[0-9]+)?)\s*s`)
)

func newPullFrames(emit func(string)) *pullFrames {
	return &pullFrames{emit: emit, last: map[string]string{}, downloaded: map[string]bool{}, frameAllDone: true}
}

// line reads one line of nerdctl's output. table reports whether it was part
// of the progress table (and so not an error message worth keeping).
func (f *pullFrames) line(raw string) (table bool) {
	line := strings.TrimSpace(pullControlRE.ReplaceAllString(raw, ""))
	if line == "" {
		return true
	}
	if m := pullElapsedRE.FindStringSubmatch(line); m != nil {
		elapsed, _ := strconv.ParseFloat(m[1], 64)
		f.endFrame(elapsed)
		return true
	}
	m := pullRowRE.FindStringSubmatch(line)
	if m == nil {
		f.emit(line)
		return false
	}
	key, status := m[1], m[2]
	blob := pullBlobKeyRE.FindStringSubmatch(key)
	if blob == nil {
		// The reference's own row (resolving/resolved).
		if status != "resolving" && status != "resolved" {
			f.notUnderstood("reference status "+strconv.Quote(status), line)
		}
		return true
	}
	kind, hex := blob[1], blob[2]
	f.frameRows++
	if kind == "layer" {
		f.frameLayer = true
	}
	var out string
	switch status {
	case "done":
		out = runtime.PullLineDownloadComplete(hex)
	case "exists":
		out = runtime.PullLineAlreadyExists(hex)
	case "waiting":
		// A blob that was downloading and is now neither in containerd's
		// active ingests nor in its content store shows as waiting again;
		// nerdctl 2.1.6 on containerd 2.x was seen to do this for the whole
		// unpack of a layer whose download had finished. Its bytes are all
		// here. A blob never seen downloading is still queued.
		if !f.downloaded[hex] {
			f.frameAllDone = false
			break
		}
		out = runtime.PullLineDownloadComplete(hex)
	case "downloading", "uploading":
		f.frameAllDone = false
		f.downloaded[hex] = true
		if b := pullRowBytesRE.FindStringSubmatch(line); b != nil {
			done, total := pullRowBytes(b[1], b[2]), pullRowBytes(b[3], b[4])
			// A row still downloading is not finished, however the rounding
			// to one decimal shows it: the stall watch must keep judging it.
			if total > 0 && done >= total {
				done = total - 1
			}
			out = runtime.PullLineDownloadingInSteps(hex, done, total, pullRowStep(b[2]))
		} else {
			f.notUnderstood("byte count", line)
		}
	case "committing":
		f.frameAllDone = false
	default:
		// A status this code does not know: whether the blob is still
		// downloading cannot be told, so its last count must not be judged.
		f.frameAllDone = false
		f.notUnderstood("blob status "+strconv.Quote(status), line)
	}
	if out != "" && f.last[hex] != out {
		f.last[hex] = out
		f.emit(out)
	}
	return true
}

// notUnderstood declares, once per pull, that nerdctl printed a table row this
// code cannot read (a newer nerdctl, most likely): the stall watch is turned
// off for the pull rather than left judging a count that may only have stopped
// being shown. The row itself goes to the agent log for whoever updates this
// parser.
func (f *pullFrames) notUnderstood(what, row string) {
	if f.unreadable {
		return
	}
	f.unreadable = true
	slog.Warn("nerdctl pull progress not understood; stall check off for this pull", "what", what, "row", row)
	f.emit(runtime.PullLineUnreadable("nerdctl " + what))
}

func (f *pullFrames) endFrame(elapsed float64) {
	allDone := f.frameLayer && f.frameAllDone && f.frameRows > 0
	f.frameLayer, f.frameAllDone, f.frameRows = false, true, 0
	if !allDone {
		f.unpacking = false
		return
	}
	if !f.unpacking {
		f.unpacking, f.unpackSince, f.reported = true, elapsed, 0
		f.emit(runtime.PullLineExtractingLayers(0))
		return
	}
	if s := int(elapsed - f.unpackSince); s/pullUnpackReportEvery > f.reported/pullUnpackReportEvery {
		f.reported = s
		f.emit(runtime.PullLineExtractingLayers(s))
	}
}

// pullRowBytes converts nerdctl's "%02.1f" value and the first letter of its
// IEC unit (containerd pkg/progress.Bytes) to bytes.
func pullRowBytes(value, prefix string) int64 {
	v, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return int64(v * pullUnitBytes(prefix))
}

// pullRowStep is how far a count shown with this unit prefix moves between two
// values nerdctl can print: one decimal of the unit. Plain bytes have none
// worth naming.
func pullRowStep(prefix string) int64 {
	return int64(pullUnitBytes(prefix) / 10)
}

func pullUnitBytes(prefix string) float64 {
	v := 1.0
	if i := strings.Index("KMGTPEZY", prefix); prefix != "" && i >= 0 {
		for n := 0; n <= i; n++ {
			v *= 1024
		}
	}
	return v
}
