package uxrecord

import "gpuworks.ai/gpuworks/internal/server/tracecontext"

// Live is a snapshot of one request the gateway is forwarding right now.
// Times are milliseconds since the gateway received it; Unknown when not
// yet. AgeMS is how long ago that was.
type Live struct {
	ID            string
	TraceID       tracecontext.TraceID
	DeploymentID  uint
	InstanceUUID  string
	APIKeyID      uint
	UserID        uint
	Source        Source
	ReceivedMS    int64
	AgeMS         int64
	Stream        bool
	BodyBytes     int64
	Images        int32
	DispatchMS    int32
	FirstThinkMS  int32
	FirstAnswerMS int32
	LastEventMS   int32
	Events        int32
	ThinkEvents   int32
	// PDPrefillInstance and PDPrefillStartMS describe a PD request's
	// prefill stage: the prefill instance it went to and when the stage
	// began. The start counts only with an instance: "" is any other
	// request. A request still in the stage has no DispatchMS yet.
	PDPrefillInstance string
	PDPrefillStartMS  int32
}

// LiveSnapshot is a deployment's in-flight requests: the counts of every
// phase, and up to limit requests, most worth a look first - those still
// waiting for their first token (longest wait first), then those thinking,
// then those answering (oldest first).
type LiveSnapshot struct {
	Waiting, Thinking, Answering int
	Requests                     []Live
}

// Phases of a live request as the gateway sees it: no token yet (queued
// or prefilling - the gateway cannot tell which), reasoning so far, or
// answering.
const (
	PhaseWaiting   = "waiting"
	PhaseThinking  = "thinking"
	PhaseAnswering = "answering"
)

// Phase is the request's phase.
func (r Live) Phase() string {
	switch {
	case r.FirstAnswerMS >= 0:
		return PhaseAnswering
	case r.FirstThinkMS >= 0:
		return PhaseThinking
	default:
		return PhaseWaiting
	}
}
