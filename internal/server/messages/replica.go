package messages

// Replica product error codes: a deployment's replicas and the orders that
// name one, decided in package planning and passed on by the api layer.
const (
	// ErrorReplicasTooMany: more replicas than a deployment may run
	// ({maximum}).
	ErrorReplicasTooMany = "REPLICAS_TOO_MANY"
	// ErrorReplicaUnitRanks: a multi-node replica does not span the
	// deployment's rank count.
	ErrorReplicaUnitRanks = "REPLICA_UNIT_RANKS"
	// ErrorReplicaUnitNode: a replica names an unknown node, or one node twice.
	ErrorReplicaUnitNode = "REPLICA_UNIT_NODE"
	// ErrorReplicaUnitNodeOffline: a replica names a node that is not online.
	ErrorReplicaUnitNodeOffline = "REPLICA_UNIT_NODE_OFFLINE"
	// ErrorReplicaUnitGPUs: a multi-node replica pins GPUs.
	ErrorReplicaUnitGPUs = "REPLICA_UNIT_GPUS"
	// ErrorReplicaUnitGPUsInsufficient: the ranks listed on a node need more
	// GPUs than it has.
	ErrorReplicaUnitGPUsInsufficient = "REPLICA_UNIT_GPUS_INSUFFICIENT"
	// ErrorReplicaUnitRequirements: a replica names a node the deployment's
	// node requirements exclude.
	ErrorReplicaUnitRequirements = "REPLICA_UNIT_REQUIREMENTS"
	// ErrorReplicaLeaderShared: two replicas are led from one node.
	ErrorReplicaLeaderShared = "REPLICA_LEADER_SHARED"
	// ErrorInstanceNotServing: an order names an instance that fills no
	// replica ({state}).
	ErrorInstanceNotServing = "INSTANCE_NOT_SERVING"
	// ErrorSleepRoleCannotEmpty: a sleep order would leave a PD role the mode
	// cannot run without with no awake instance ({role}).
	ErrorSleepRoleCannotEmpty = "SLEEP_ROLE_CANNOT_EMPTY"
	// ErrorReplicasTooFew: a replica count below 1 ({minimum}); stopping
	// the deployment runs none.
	ErrorReplicasTooFew = "REPLICAS_TOO_FEW"
	// ErrorReplicasRemoveInvalid: a named instance does not fill a replica of
	// the deployment ({instance}).
	ErrorReplicasRemoveInvalid = "REPLICAS_REMOVE_INVALID"
	// ErrorReplicasRemoveTooMany: more instances named than replicas removed
	// ({count}, {maximum}).
	ErrorReplicasRemoveTooMany = "REPLICAS_REMOVE_TOO_MANY"
	// ErrorReplicasPlaceOnTooMany: more nodes named than the new replicas
	// have ranks ({count}, {maximum}).
	ErrorReplicasPlaceOnTooMany = "REPLICAS_PLACE_ON_TOO_MANY"
	// ErrorReplicasPlaceOnRefused: a node named for a new replica cannot
	// carry it now ({node}, {reason}, {detail}).
	ErrorReplicasPlaceOnRefused = "REPLICAS_PLACE_ON_REFUSED"
)
