package proto

// Why Slurm will not start jobs on a node (SysInfo.SlurmUnavailable), in the
// order the agent decides them: the first that applies is the one reported.
const (
	// SlurmNodeGRESMissing: slurmd registered fewer GPUs than the controller
	// lists for the node, which is how a missing gres.conf shows.
	SlurmNodeGRESMissing = "gres_missing"
	// SlurmNodeInvalidRegistration: the node's registration was refused for
	// another reason.
	SlurmNodeInvalidRegistration = "invalid_registration"
	// SlurmNodeDown: the node is down, failed, or does not answer.
	SlurmNodeDown = "down"
	// SlurmNodeDrained: the node is draining or drained.
	SlurmNodeDrained = "drained"
)

// ErrorSlurmEnrollRefused is the product-error code of a refused Slurm
// enrollment (POST /api/v1/slurm/enroll, 403): the server answers
// {"code":"SLURM_ENROLL_REFUSED","params":{"reason":...,"entry":...}} with
// reason unknown_token (the token is unknown or revoked), other_cluster (the
// token is bound to another Slurm cluster), node_taken (another entry of this
// Slurm cluster holds the node; entry names it) or EnrollReasonRefused (the
// fleet takes no new node now; the node is taken once there is room).
const ErrorSlurmEnrollRefused = "SLURM_ENROLL_REFUSED"

// The reasons of an ErrorSlurmEnrollRefused answer.
const (
	SlurmEnrollUnknownToken = "unknown_token"
	SlurmEnrollOtherCluster = "other_cluster"
	SlurmEnrollNodeTaken    = "node_taken"
)

// EnrollReasonRefused is the reason of a refused enrollment, Kubernetes
// (KUBE_ENROLL_REFUSED) or Slurm alike, when the fleet takes no new node
// now: an enrollment guard refused it, and params.refusal_code names why.
const EnrollReasonRefused = "enrollment_refused"
