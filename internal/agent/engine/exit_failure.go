package engine

import (
	"context"
	"io"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/commandlog"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/enginelog"
)

// exitTailLines is how much of a stopped container's output is read for the
// reason it stopped. A tensor-parallel vLLM prints each worker's traceback
// before the engine core's and the API server's wrappers, so the root cause
// can sit a few hundred lines above the end.
const exitTailLines = 1000

// exitTailBytes bounds how much of those lines is kept: their end, since the
// reason a container stopped is the last thing it printed.
const exitTailBytes = 512 * 1024

// exitOutput is what a stopped container's output says about why it
// stopped: the known failures it shows (enginelog.Classify) and the error the
// engine's failure began with (enginelog.RootCause, which looks past "See
// root cause above" wrappers). Removal deletes the output, so this is what is
// left of it on the failed instance.
type exitOutput struct {
	causes []enginelog.Cause
	root   string
	// tail is the output's last lines, for a startup exit that named no
	// error: what the container said before it gave up is then the reason
	// there is.
	tail []string
	// waiting is the last lines with a line repeated in a row counted once,
	// for a startup that timed out without naming an error: a process that
	// hangs repeats one line, and what it printed before is where it stopped.
	waiting []string
}

// readExitOutput reads it for an instance whose dGPU memory limit is
// vramLimitBytes (0: a whole GPU): under a limit, the engine's own report of
// running out of memory on a device as large as the limit is the limit.
func (e *Engine) readExitOutput(ctx context.Context, id string, vramLimitBytes int64) (exitOutput, bool) {
	rc, err := e.Driver().Logs(ctx, id, false, exitTailLines)
	if err != nil {
		return exitOutput{}, false
	}
	defer rc.Close()
	text, err := readEnd(rc, exitTailBytes)
	if err != nil {
		return exitOutput{}, false
	}
	out := exitOutput{causes: enginelog.ClassifyWithLimit(text, vramLimitBytes), root: commandlog.Redact(enginelog.RootCause(text))}
	// These lines are whatever the container printed last, not lines chosen
	// for naming an error: a launch script tracing its commands ends on a
	// command line, and what it passed as a credential is not for the
	// failure message, the timeline or anything read from them.
	for _, line := range enginelog.Tail(text, proto.MaxFailureEvidence) {
		out.tail = append(out.tail, commandlog.Redact(line))
	}
	for _, line := range enginelog.TailDistinct(text, proto.MaxFailureEvidence) {
		out.waiting = append(out.waiting, commandlog.Redact(line))
	}
	return out, true
}

// readEnd reads r to its end and returns the last limit bytes of it, from
// the start of a line when the output was longer than that.
func readEnd(r io.Reader, limit int) (string, error) {
	kept := make([]byte, 0, limit)
	buf := make([]byte, 32*1024)
	cut := false
	for {
		n, err := r.Read(buf)
		kept = append(kept, buf[:n]...)
		if over := len(kept) - limit; over > 0 {
			kept = append(kept[:0], kept[over:]...)
			cut = true
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if cut {
		for i, b := range kept {
			if b == '\n' || b == '\r' {
				kept = kept[i+1:]
				break
			}
		}
	}
	return string(kept), nil
}

// attach puts the output's causes and root-cause line on failure.
func (o exitOutput) attach(failure *proto.RuntimeFailure) {
	for _, c := range o.causes {
		cause := proto.FailureCause{Code: c.Code, Evidence: c.Lines(), GLIBC: c.GLIBC}
		if c.Code == enginelog.CodeDGPUMemoryLimit {
			cause.NeededBytes, cause.LimitBytes = c.NeededBytes, c.LimitBytes
			if device := c.Device; device >= 0 {
				cause.Device = &device
			}
		}
		failure.Causes = append(failure.Causes, cause)
	}
	switch {
	case o.root != "":
		failure.Evidence = []string{o.root}
	case len(o.tail) > 0:
		failure.Evidence = o.tail
	}
}

// message is the English fallback for an exit: a missing glibc replaces it
// (the exit status says nothing more), a dGPU limit refusal or the root-cause
// line is added to base (a limit the output gave no numbers for keeps the
// engine's own line beside it: an error code is then all there is to go on),
// a startup exit that named no error adds its last line, and an exit that
// printed nothing says so, which a full disk also produces.
func (o exitOutput) message(base string) string {
	if c, ok := o.cause(enginelog.CodeGLIBCTooOld); ok {
		return c.Fallback()
	}
	if c, ok := o.cause(enginelog.CodeDGPUMemoryLimit); ok {
		if c.NeededBytes <= 0 && o.root != "" {
			return base + ": " + c.Fallback() + " (" + o.root + ")"
		}
		return base + ": " + c.Fallback()
	}
	if o.root != "" {
		return base + ": " + o.root
	}
	if len(o.tail) > 0 {
		return base + ": " + o.tail[len(o.tail)-1]
	}
	if _, ok := o.cause(enginelog.CodeNoOutput); ok {
		return base + " with no output"
	}
	return base
}

func (o exitOutput) cause(code string) (enginelog.Cause, bool) {
	for _, c := range o.causes {
		if c.Code == code {
			return c, true
		}
	}
	return enginelog.Cause{}, false
}

// exitedFailure is the reason a container that stopped on its own stopped,
// in phase: killed for memory (oomMessage is the fallback) or exited
// (exitBase is the fallback the output's reason is added to). Either way the
// output's known failures and root cause go with it, read against the
// instance's dGPU memory limit.
func (e *Engine) exitedFailure(ctx context.Context, id string, st runtime.Status, vramLimitBytes int64, phase, oomMessage, exitBase string) (proto.RuntimeFailure, string) {
	exitCode := st.ExitCode
	failure := proto.RuntimeFailure{Code: proto.ErrorContainerExited, Phase: phase, ExitCode: &exitCode}
	oom := e.detectOOM(ctx, id, st)
	if oom {
		failure.Code, failure.OOMKilled = proto.ErrorContainerOOMKilled, true
	}
	out, _ := e.readExitOutput(ctx, id, vramLimitBytes)
	if oom || phase == proto.PhaseServing || len(out.causes) > 0 {
		// A kill for memory and a known failure are their own reason (the
		// known failure carries its lines as its evidence), and an engine
		// that stopped while serving ends on ordinary lines that say nothing
		// about why.
		out.tail = nil
	}
	out.attach(&failure)
	if oom {
		return failure, oomMessage
	}
	return failure, out.message(exitBase)
}

// attachOutput adds what a container that is still there printed to the
// reason it is failed for (a startup that timed out, an unhealthy worker):
// it is reclaimed next, and its output with it. Silence from a process that
// is still running is not the exit without output CodeNoOutput names.
//
// The process has not exited, so its last line is not a reason and never
// joins the message. A startup that timedOut and showed no known failure and
// no error line still carries its last lines as evidence: they are all there
// is to say where it hung.
func (e *Engine) attachOutput(ctx context.Context, id string, vramLimitBytes int64, failure *proto.RuntimeFailure, timedOut bool) {
	out, read := e.readExitOutput(ctx, id, vramLimitBytes)
	if !read {
		return
	}
	if _, silent := out.cause(enginelog.CodeNoOutput); silent {
		out.causes = nil
	}
	out.tail = nil
	if timedOut && len(out.causes) == 0 {
		out.tail = out.waiting
	}
	out.attach(failure)
}
