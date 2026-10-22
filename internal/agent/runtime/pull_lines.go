package runtime

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A driver that cannot pass its runtime's own pull output through (nerdctl's
// redrawn table, udocker's storage counters, the native udocker pipeline)
// reports progress to PullImage's onLine with the lines below. They are a
// subset of docker's own pull vocabulary, which the node's progress parser
// (imagepull.Progress) also reads; imagepull's contract test feeds every one of
// them to that parser.
//
// Every line about a layer is keyed "sha256:<hex>:". A driver passes the same
// digest for a layer on every line, so that a later line finishes what an
// earlier one started: a count the parser cannot tie to a layer is shown but
// never judged.

// PullLineDownloading reports a layer's bytes received so far. done equal to
// total marks the layer as downloaded.
func PullLineDownloading(digest string, done, total int64) string {
	return pullLineBytes(digest, "Downloading", done, total)
}

// PullLineDownloadingInSteps is PullLineDownloading for a counter the runtime
// shows only in steps of step bytes: an unchanged count proves no stall until
// the download has had time to cover one step. done must stay below total
// while the layer is still downloading.
func PullLineDownloadingInSteps(digest string, done, total, step int64) string {
	line := PullLineDownloading(digest, done, total)
	if step > 0 {
		line += fmt.Sprintf(" (in steps of %d B)", step)
	}
	return line
}

// PullLineStep reads the step PullLineDownloadingInSteps put on a line; zero
// for any other line.
func PullLineStep(line string) int64 {
	m := pullLineStepRE.FindStringSubmatch(line)
	if m == nil {
		return 0
	}
	step, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0
	}
	return step
}

var pullLineStepRE = regexp.MustCompile(`: Downloading [0-9]+ B/[0-9]+ B \(in steps of ([0-9]+) B\)$`)

// PullLineUnreadable says the driver met progress output it does not
// understand; what names it (a status word, never the raw row, so that no
// byte count or phase word in it is read as progress). From then on the pull
// is not judged for stalls: a count the driver can no longer follow may only
// have stopped being shown, and judging it would give up on a pull that is
// still running (CandidateTimeout still bounds it). A runtime update that
// changes its output therefore costs the stall check, never the pull.
func PullLineUnreadable(what string) string {
	return pullLineUnreadablePrefix + what
}

// IsPullLineUnreadable reports whether line came from PullLineUnreadable.
func IsPullLineUnreadable(line string) bool {
	return strings.HasPrefix(line, pullLineUnreadablePrefix)
}

// pullLineUnreadablePrefix holds none of the words the progress parser reads
// a phase from (downloading, extracting, complete, exists).
const pullLineUnreadablePrefix = "Progress output not understood, stall check off: "

// PullLineExtracting reports a layer's bytes unpacked so far.
func PullLineExtracting(digest string, done, total int64) string {
	return pullLineBytes(digest, "Extracting", done, total)
}

// PullLineDownloadComplete reports a layer whose bytes are all here.
func PullLineDownloadComplete(digest string) string {
	return pullLineKey(digest) + " Download complete"
}

// PullLineAlreadyExists reports a layer the node already had.
func PullLineAlreadyExists(digest string) string {
	return pullLineKey(digest) + " Already exists"
}

// PullLineExtractingLayers reports that the layers are downloaded and being
// unpacked, for a runtime that says so for the image as a whole; seconds is how
// long that has gone on, or zero when it begins.
func PullLineExtractingLayers(seconds int) string {
	if seconds <= 0 {
		return "Extracting downloaded layers"
	}
	return fmt.Sprintf("Extracting downloaded layers (%ds)", seconds)
}

func pullLineBytes(digest, verb string, done, total int64) string {
	return fmt.Sprintf("%s %s %d B/%d B", pullLineKey(digest), verb, done, total)
}

// pullLineKey is the layer key, "sha256:<hex>:", the same whether the digest
// came with its algorithm or without.
func pullLineKey(digest string) string {
	return "sha256:" + strings.TrimPrefix(digest, "sha256:") + ":"
}
