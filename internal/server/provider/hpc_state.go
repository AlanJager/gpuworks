package provider

import "strings"

// The stable codes of an HPC cluster's state, as the API tells them apart from
// the vendor's own words. They are the states E-HPC documents for a cluster
// (ListClusters and GetCluster, ClusterStatus); a word outside the list is
// HPCStateUnknown, so a client never shows or branches on a vendor spelling.
const (
	HPCStateUninit    = "uninit"    // installing
	HPCStateCreating  = "creating"  // being created
	HPCStateIniting   = "initing"   // initializing
	HPCStatePending   = "pending"   // waiting to be configured
	HPCStateRunning   = "running"   // usable
	HPCStateException = "exception" // failed, needs the vendor's console
	HPCStateStopping  = "stopping"
	HPCStateStopped   = "stopped"
	HPCStateReleasing = "releasing" // being deleted
	HPCStateUnknown   = "unknown"
)

// HPCStateCode is the stable code of a vendor state word.
func HPCStateCode(word string) string {
	switch w := strings.ToLower(strings.TrimSpace(word)); w {
	case HPCStateUninit, HPCStateCreating, HPCStateIniting, HPCStatePending, HPCStateRunning,
		HPCStateException, HPCStateStopping, HPCStateStopped, HPCStateReleasing:
		return w
	case "raleasing": // GetCluster's documentation spells it so
		return HPCStateReleasing
	}
	return HPCStateUnknown
}

// The tones of a state, which are the status colours of a badge: success,
// info (on its way), neutral (nothing to do right now) and danger.
const (
	HPCToneSuccess = "success"
	HPCToneInfo    = "info"
	HPCToneNeutral = "neutral"
	HPCToneDanger  = "danger"
)

// HPCStateTone is the tone a state is shown in.
func HPCStateTone(code string) string {
	switch code {
	case HPCStateRunning:
		return HPCToneSuccess
	case HPCStateUninit, HPCStateCreating, HPCStateIniting:
		return HPCToneInfo
	case HPCStateException:
		return HPCToneDanger
	}
	return HPCToneNeutral
}

// Why a Slurm cluster with no entry cannot be connected yet, because of the
// state it is in; each says what happens next, which the client words.
const (
	// HPCNotConnectableStarting: it is being created or initialized and can
	// be connected once it runs.
	HPCNotConnectableStarting = "cluster_starting"
	// HPCNotConnectablePending: it is waiting to be configured.
	HPCNotConnectablePending = "cluster_pending"
	// HPCNotConnectableException: it failed; it has to be dealt with in the
	// vendor's console before it can be connected.
	HPCNotConnectableException = "cluster_exception"
	// HPCNotConnectableStopped: it is stopped or stopping and has to be
	// started in the vendor's console.
	HPCNotConnectableStopped = "cluster_stopped"
	// HPCNotConnectableReleasing: it is being deleted and cannot be connected.
	HPCNotConnectableReleasing = "cluster_releasing"
	// HPCNotConnectableUnknown: the vendor reported a state not known here.
	HPCNotConnectableUnknown = "cluster_state_unknown"
)

// HPCNotConnectableReason is why a cluster in the state is not connected yet,
// "" for a running one.
func HPCNotConnectableReason(code string) string {
	switch code {
	case HPCStateRunning:
		return ""
	case HPCStateUninit, HPCStateCreating, HPCStateIniting:
		return HPCNotConnectableStarting
	case HPCStatePending:
		return HPCNotConnectablePending
	case HPCStateException:
		return HPCNotConnectableException
	case HPCStateStopping, HPCStateStopped:
		return HPCNotConnectableStopped
	case HPCStateReleasing:
		return HPCNotConnectableReleasing
	}
	return HPCNotConnectableUnknown
}
