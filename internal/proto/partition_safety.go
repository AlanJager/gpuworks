package proto

// The levels of the partition safety contract an agent states in
// SysInfo.PartitionSafetyProtocol.
const (
	// PartitionSafetyOwnership: the agent keeps a partition's native state
	// and owner until its container is confirmed gone, which a backend
	// without a behavioural probe relies on (partitioncap).
	PartitionSafetyOwnership = 1
	// PartitionSafetyForeignTracked: the agent keeps reporting the processes
	// outside the platform it found on a card after one of the platform's
	// instances joins them there, for as long as that instance stays (at
	// their last figure once they are no longer listed: they may have
	// restarted under a new PID). Without that, the first partition placed
	// beside them would make the card read as free, so the server shares a
	// held card by memory only with an agent at this level (ADR 0068).
	// Rolling an agent back below it stops new sharing only: partitions
	// already beside such processes lose their cover, since the older agent
	// stops reporting them.
	PartitionSafetyForeignTracked = 2
	// PartitionSafetyCurrent is the level this build's agent keeps.
	PartitionSafetyCurrent = PartitionSafetyForeignTracked
)
