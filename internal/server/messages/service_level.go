package messages

// Product error codes of service levels: how a deployment shares GPUs with
// others (internal/server/servicelevel).
const (
	// ErrorServiceLevelInvalid: the level is none of resident, online,
	// shared or offline ({level}).
	ErrorServiceLevelInvalid = "SERVICE_LEVEL_INVALID"
	// ErrorServiceLevelShape: a level other than resident is for a
	// single-node deployment; a multi-node or PD one stays resident.
	ErrorServiceLevelShape = "SERVICE_LEVEL_SHAPE"
	// ErrorServiceLevelOnlineShared: the deployment shares cards with
	// another online deployment ({deployment}); two online deployments
	// would each hold the other's requests while busy.
	ErrorServiceLevelOnlineShared = "SERVICE_LEVEL_ONLINE_SHARED"
)
