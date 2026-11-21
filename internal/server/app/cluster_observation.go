package app

import (
	"context"
	"errors"
	"time"
)

// ClusterObservation is what one member sees of its control-plane cluster at
// one moment: who the members are, which answer, and whether the cluster can
// lose another. It carries the facts and verdicts of the status report
// (`cluster-status`) as values a module can serve; the report's own lines are
// worded from the same verdicts, so the two cannot disagree.
//
// Clustered is false for a single server and for a build without the cluster
// runtime; every other field is then zero except ObservedAt.
type ClusterObservation struct {
	Clustered  bool
	ObservedAt time.Time
	// ProbeTimeout is the deadline each probe of this observation had.
	ProbeTimeout time.Duration
	// LeaderID is the database leader the members reported, zero when none
	// was observed.
	LeaderID uint64
	// Voters counts the voters in the membership this observation used.
	Voters int
	// FaultTolerance says whether the cluster still serves after one more
	// member fails.
	FaultTolerance ClusterFaultTolerance
	// Forming is true while the cluster's newest admission may still be on
	// its way into the membership, or an installation may still be starting
	// its members. A forming cluster is not reported as short of anything.
	Forming bool
	// Shortfall says why a cluster whose business answers is not at full
	// strength; its Kind is ClusterShortfallNone when it is, or when that
	// cannot be told.
	Shortfall ClusterShortfall
	Members   []ClusterObservedMember
}

// ClusterFaultTolerance is the verdict of the report's "Fault tolerant" line.
type ClusterFaultTolerance string

const (
	// ClusterTolerant: the cluster keeps a majority after losing one more voter.
	ClusterTolerant ClusterFaultTolerance = "tolerant"
	// ClusterNotTolerant: losing one more voter stops the cluster.
	ClusterNotTolerant ClusterFaultTolerance = "not_tolerant"
	// ClusterToleranceUnverified: a voter gave no answer in time, so the
	// question is open.
	ClusterToleranceUnverified ClusterFaultTolerance = "unverified"
)

// ClusterShortfallKind names what a cluster lacks.
type ClusterShortfallKind string

const (
	ClusterShortfallNone ClusterShortfallKind = ""
	// ClusterShortfallFewVoters: fewer than three voters among the members.
	ClusterShortfallFewVoters ClusterShortfallKind = "few_voters"
	// ClusterShortfallVotersMissing: three voters or more, and some are
	// unreachable or gave no answer in time.
	ClusterShortfallVotersMissing ClusterShortfallKind = "voters_missing"
)

// ClusterShortfall is the reason behind a degraded verdict. Voters and
// Members are set for every kind but none; Unreachable and Silent count the
// voters that refused a connection (or whose host is unreachable) and the ones
// that gave no answer within the probe timeout.
type ClusterShortfall struct {
	Kind        ClusterShortfallKind
	Voters      int
	Members     int
	Unreachable int
	Silent      int
}

// ClusterObservedMember is one member of the observed membership.
type ClusterObservedMember struct {
	ID      uint64
	Address string
	// Self marks the member that took the observation.
	Self bool
	// Role is the raft role: "voter", "stand-by" or "spare".
	Role string
	// Live is true when the role came from the leader's membership and false
	// when only this member's cached membership file names the member.
	Live bool
	// Database is what the member's database port did when this member
	// connected: "open", "connection refused", "timed out", "unreachable",
	// or "not observed".
	Database string
	// Listener is what the member's enrollment listener did: "reached
	// process", "refused", "timed out", "unreachable" or "not observed". For
	// Self it reports the local listener the same way.
	Listener string
}

// ErrClusterObservationTimeout is returned for a non-positive probe timeout.
var ErrClusterObservationTimeout = errors.New("cluster observation requires a positive probe timeout")

// ClusterObservation observes the control-plane cluster this server belongs
// to, as `cluster-status` does: it reads the member's own files and probes
// the other members' listeners, each probe bounded by timeout, one after the
// other. It changes nothing. A server that is not a cluster member answers
// with Clustered false and no error. Unlike the registrations it may be called
// after Init, and is meant to be called while serving.
func (h *Host) ClusterObservation(ctx context.Context, timeout time.Duration) (ClusterObservation, error) {
	if timeout <= 0 {
		return ClusterObservation{}, ErrClusterObservationTimeout
	}
	if err := ctx.Err(); err != nil {
		return ClusterObservation{}, err
	}
	return observeCluster(ctx, h.cfg, timeout)
}
