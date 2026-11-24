//go:build linux && dqlite

package store

// The policies the cluster's callers apply, side by side. They are named for
// the question that was asked, not for the caller.

// grantVerdict is what no repeat or wait of a grant may excuse: the caller's
// cancellation and every refusal of this member or of the cluster's state.
const grantVerdict = CauseCanceled | CauseRefused | CausePhaseChanged

// grantInterrupted is what a database leadership change does to a grant
// attempt, and a later attempt can outlive.
const grantInterrupted = CauseMembershipChanged | CauseNoLeader | CauseBadConn | CauseEOF | CauseUnexpectedEOF |
	CauseObservationTimeout | CauseNetwork

var (
	// GrantInterrupted: the database leader could not be reached or changed
	// during the grant. The grant is repeated on it.
	GrantInterrupted = AnswerPolicy{NoAnswer: grantInterrupted, Verdict: grantVerdict}

	// GrantUndecided: the grant carries no verdict on the member. It was
	// interrupted, or the database answered with a failure response, the
	// writer's busy answer among them.
	GrantUndecided = AnswerPolicy{NoAnswer: grantInterrupted | CauseFailureResponse, Verdict: grantVerdict}

	// ProofUnanswered: every cause of a failed material proof is an expired
	// deadline, a database with no leader, a leader not read in time, or a
	// connection lost under the question. Nothing was compared.
	ProofUnanswered = AnswerPolicy{
		NoAnswer:     CauseNoLeader,
		NoAnswerLeaf: CauseDeadline | CauseObservationTimeout | CauseBadConn | CauseEOF | CauseUnexpectedEOF,
		Joined:       EveryBranchUnderWrapper,
	}

	// ProofReadUnanswered: a database read the proof could not start. A
	// failure the database itself answered stays a verdict: a refusal raised
	// inside a statement arrives the same way.
	ProofReadUnanswered = AnswerPolicy{NoAnswer: CauseNoLeader | CauseObservationTimeout}

	// ProofClusterLost: the proof lost its context, its database connection
	// or the peer it was reading from.
	ProofClusterLost = AnswerPolicy{
		NoAnswer:   CauseCanceled | CauseDeadline | CauseBadConn | CauseLostConnection | CauseUnexpectedEOF,
		UnlessFile: CauseLostConnection | CauseUnexpectedEOF,
	}

	// ManifestReadUnanswered: the first read of the manifest was not answered
	// by the database. It is narrower than a recheck on purpose: this read
	// also opens the member's files, and a missing or unreadable file must
	// stay a verdict.
	ManifestReadUnanswered = AnswerPolicy{
		NoAnswer: CauseCanceled | CauseDeadline | CauseNoLeader | CauseObservationTimeout,
		Verdict:  CauseRefused | CauseNoRow,
		Joined:   EveryBranchAlone,
	}

	// ManifestRecheckInterrupted: a recheck of the manifest failed only
	// because the database or the request went away under it.
	ManifestRecheckInterrupted = AnswerPolicy{
		NoAnswer: CauseCanceled | CauseDeadline | CauseNoLeader | CauseObservationTimeout | CauseBadConn |
			CauseEOF | CauseUnexpectedEOF | CauseLostConnection,
		UnlessFile: CauseLostConnection,
		Verdict:    CauseRefused | CauseNoRow,
		Joined:     EveryBranchAlone,
	}

	// ReplicationUndecided: a replication pass that carries no verdict on
	// the member's material. The caller's cancellation is not undecided, and
	// a second cause joined to one of these keeps its own verdict.
	ReplicationUndecided = AnswerPolicy{
		NoAnswer:     CauseNoLeader | CauseObservationTimeout | CauseBadConn | CauseNoMajority,
		NoAnswerLeaf: CauseDeadline,
		Joined:       NotJoined,
	}

	// PeerUnreachable: a request to another member that delivered no answer.
	// A failed certificate or identity check is not among these.
	PeerUnreachable = AnswerPolicy{
		NoAnswer: CauseNetTimeout | CauseEOF | CauseUnexpectedEOF | CausePeerUnreachable | CauseDeadline,
		Joined:   EveryBranchAlone,
	}
)
