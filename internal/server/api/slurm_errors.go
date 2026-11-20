package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Slurm cluster codes (/slurm/clusters, /slurm/enroll), declared in package
// messages and, for the agent's enrollment, package proto.
const (
	ErrorSlurmClusterInvalid     = messages.ErrorSlurmClusterInvalid
	ErrorSlurmClusterNameTaken   = messages.ErrorSlurmClusterNameTaken
	ErrorSlurmClusterHasNodes    = messages.ErrorSlurmClusterHasNodes
	ErrorSlurmClusterSourceTaken = messages.ErrorSlurmClusterSourceTaken
	ErrorSlurmEnrollRefused      = proto.ErrorSlurmEnrollRefused
)
